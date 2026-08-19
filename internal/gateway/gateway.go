// Package gateway is the capsule's egress gateway: the process that
// runs in the only container attached to both the capsule's isolated
// bridge and the Runpool uplink.
//
// It does not route. The capsule's bridge is internal in Engine 28's
// isolated gateway mode, which makes the host kernel drop every packet
// the capsule addresses outside that subnet — a deny no privileged
// capsule can lift, because the rule is not in its namespace. What
// stays reachable is the gateway itself, so egress happens by
// relaying: the gateway resolves names for the capsule and opens
// connections on its behalf, checking every resolved address against
// the policy before it dials.
//
// Everything here is bounded on purpose. The work this process does is
// driven by a workload, so unbounded connections, goroutines or buffers
// would be a way for a job to consume resources outside its own budget.
// Every limit has a named constant and a test.
//
// Nothing is optional: a policy that will not compile, an interface
// that cannot be classified, a ruleset that will not install or a
// listener that will not bind all report failed, and the controller
// never starts a capsule whose gateway is not ready.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/egress"
)

// Config is what the gateway needs to start. The caller supplies the
// policy text (the controller passes it in the environment) and the
// directory holding the control files.
type Config struct {
	Policy     string
	ControlDir string
	Log        *slog.Logger
	// SetState reports readiness or failure through the supervisor's
	// protocol; the controller polls it and refuses to start a capsule
	// whose gateway never reported ready.
	SetState func(string)
}

// PolicyPath is the policy in force, on the gateway's tmpfs. The relay
// reloads it when it changes, which is how a reload arriving as a
// separate exec reaches this running process.
func PolicyPath(controlDir string) string {
	return filepath.Join(controlDir, "gateway-policy.json")
}

// Run is the gateway container's PID 1. It returns when ctx is done.
func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.ControlDir, 0o755); err != nil {
		return err
	}
	policy, err := ParsePolicy(cfg.Policy)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	legs, err := ClassifyLegs(policy)
	if err != nil {
		return fmt.Errorf("interfaces: %w", err)
	}
	if err := ApplyFirewall(policy, legs); err != nil {
		return fmt.Errorf("ruleset: %w", err)
	}
	// The policy in force lives in a file from here on, so a reload
	// exec and this process agree on one source.
	path := PolicyPath(cfg.ControlDir)
	if err := writeFileAtomic(path, []byte(cfg.Policy)); err != nil {
		return fmt.Errorf("policy file: %w", err)
	}
	store := &PolicyStore{Path: path}
	if _, err := store.Current(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	dns := &DNSRelay{Log: cfg.Log}
	if err := dns.Listen(ctx, legs.InternalIP); err != nil {
		return fmt.Errorf("dns relay: %w", err)
	}
	relay := &Relay{Policy: store, Log: cfg.Log}
	if err := relay.Listen(ctx, legs.InternalIP); err != nil {
		return fmt.Errorf("egress relay: %w", err)
	}

	cfg.SetState(protocol.StateReady)
	cfg.Log.Info("gateway ready",
		"internal", legs.InternalIf, "uplink", legs.UplinkIf,
		"proxy", net.JoinHostPort(legs.InternalIP, fmt.Sprint(egress.ProxyPort)),
		"deny", len(policy.Deny), "allow", len(policy.Allow),
		"max_connections", MaxProxyConnections, "max_dns_inflight", MaxDNSInFlight)
	<-ctx.Done()
	cfg.SetState(protocol.ExitedPrefix + "0")
	return nil
}

// Legs are the gateway's two interfaces, identified by matching this
// container's addresses against the policy's subnets. Interface names
// are attachment-order trivia — the daemon hands out eth0 and eth1 in
// an order that depends on when each network was connected — so the
// addresses are what classify them.
type Legs struct {
	InternalIf string
	UplinkIf   string
	InternalIP string
}

// ClassifyLegs finds which interface faces the capsule and which faces
// the uplink. Failing to find both is fatal: a gateway that cannot tell
// its legs apart cannot write a correct ruleset.
func ClassifyLegs(p egress.Policy) (Legs, error) {
	internal, err := netip.ParsePrefix(p.InternalSubnet)
	if err != nil {
		return Legs{}, err
	}
	uplink, err := netip.ParsePrefix(p.UplinkSubnet)
	if err != nil {
		return Legs{}, err
	}
	var l Legs
	ifaces, err := net.Interfaces()
	if err != nil {
		return l, err
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return l, err
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(v4)
			if !ok {
				continue
			}
			if internal.Contains(addr) {
				l.InternalIf, l.InternalIP = iface.Name, addr.String()
			}
			if uplink.Contains(addr) {
				l.UplinkIf = iface.Name
			}
		}
	}
	if l.InternalIf == "" || l.UplinkIf == "" {
		return l, fmt.Errorf("not attached to both networks (internal %q, uplink %q)", l.InternalIf, l.UplinkIf)
	}
	return l, nil
}

// writeFileAtomic replaces a control file by rename, so a reader never
// observes a half-written policy.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
