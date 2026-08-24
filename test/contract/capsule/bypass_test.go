package capsulecontract

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/engine/docker"

	"github.com/rhobuild/runpool/internal/assignment"
)

// TestNetworkSandboxBypass is the egress bypass suite, run against a
// real sandboxed capsule on a real daemon. The claim under test has two
// halves, and this proves both from inside the capsule's own namespace:
//
//   - Nothing routes out. The capsule's bridge is internal in isolated
//     gateway mode, so the host kernel drops every packet the capsule
//     addresses beyond that bridge — public addresses included. A
//     privileged capsule cannot lift that rule, because the rule is not
//     in its namespace.
//   - What does work is the relay, and only within its policy: the
//     gateway resolves and connects on the capsule's behalf, refusing
//     any address the policy denies, so private ranges, the host, the
//     uplink, other Docker networks and metadata services stay
//     unreachable by name as well as by address.
//
// Then the gateway is removed, and egress must go with it.
func TestNetworkSandboxBypass(t *testing.T) {
	m, dock, name := launcher(t)
	leaseID := assignment.LeaseID(name)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	rec := &memRecorder{}

	// The uplink stands in for the instance's shared egress network.
	uplinkID, err := dock.CreateNetwork(ctx, engine.NetworkSpec{
		Name:   string(string(leaseID) + "-uplink"),
		Labels: engine.Ownership{Instance: "contract", Role: engine.RoleUplink}.Labels(),
	})
	if err != nil {
		t.Fatalf("uplink: %v", err)
	}
	t.Cleanup(func() { cleanupSandbox(t, dock, rec, uplinkID) })
	uplinkSubnet, err := dock.NetworkSubnet(ctx, uplinkID)
	if err != nil {
		t.Fatal(err)
	}

	// Real serve discovers host and Docker subnets; stating them here
	// keeps the assertions independent of whatever this host runs.
	deny := egress.BuildDeny(uplinkSubnet,
		[]string{"192.168.0.0/16"}, // stand-in host LAN
		[]string{"172.30.0.0/16"})  // stand-in other Docker network

	prepared, err := m.Prepare(ctx, capsule.Spec{
		LeaseID:      assignment.LeaseID(leaseID),
		InstanceID:   "contract",
		CapsuleImage: image,
		JITConfig:    fakeJITConfig,
		Resources:    config.Resources{Memory: 2 << 30, Swap: 0, CPU: 2e9, PIDs: 512},
		CgroupDriver: cgroupDriver(ctx, t, dock),
		Sandbox: &capsule.Sandbox{
			UplinkNetworkID: uplinkID,
			UplinkSubnet:    uplinkSubnet,
			Deny:            deny,
		},
	}, rec)
	if err != nil {
		t.Fatalf("prepare sandboxed capsule: %v", err)
	}
	capsuleID := prepared.RuntimeID
	// direct opens a raw TCP connection from the capsule, with no
	// proxy: the path the host must refuse in every case. An exec that
	// fails is a broken probe, not a denial — counted as denied, a
	// missing timeout binary or a bash without /dev/tcp turned all nine
	// refusal assertions below into a suite that proves nothing.
	direct := func(host string, port int) bool {
		code, _, err := dock.Exec(ctx, string(capsuleID), []string{
			"bash", "-c", fmt.Sprintf("timeout 5 bash -c 'echo > /dev/tcp/%s/%d' 2>/dev/null", host, port),
		})
		if err != nil {
			t.Fatalf("the direct probe could not run against %s:%d: %v", host, port, err)
		}
		return code == 0
	}

	// The probe proves itself before any refusal rests on it. The
	// gateway's proxy port accepts connections from the capsule's own
	// subnet, so the exact mechanism every denial below uses — bash,
	// /dev/tcp, timeout — must connect somewhere it is allowed to. A
	// probe that answers "refused" for every address, whatever the
	// reason, dies here instead of green-lighting an absent sandbox.
	control := `h=${http_proxy#http://}; h=${h%%:*}; timeout 5 bash -c "echo > /dev/tcp/$h/3128"`
	if code, out, err := dock.Exec(ctx, string(capsuleID), []string{"bash", "-c", control}); err != nil || code != 0 {
		t.Fatalf("the positive control failed (exit %d, %v, %s): the probe cannot reach the "+
			"gateway it is allowed to reach, so its refusals below would prove nothing", code, err, out)
	}
	// relayed asks the gateway to reach a URL. Exit 0 means the relay
	// connected; the body is irrelevant.
	relayed := func(url string) (bool, string) {
		code, out, err := dock.Exec(ctx, string(capsuleID), []string{
			"bash", "-c", fmt.Sprintf(
				"curl -sS -o /dev/null -w '%%{http_code}' --max-time 20 %s 2>&1", url),
		})
		return err == nil && code == 0, strings.TrimSpace(out)
	}

	// Nothing routes out — not even to a public address. This is the
	// kernel's answer, not the gateway's.
	for _, d := range []struct {
		name string
		host string
		port int
	}{
		{"public internet, unproxied", "1.1.1.1", 443},
		{"cloud metadata / link-local", "169.254.169.254", 80},
		{"RFC1918 host LAN", "192.168.1.1", 22},
		{"RFC1918 10-space", "10.0.0.1", 22},
		{"another Docker network", "172.30.0.1", 80},
		{"the uplink subnet itself", firstHost(uplinkSubnet), 80},
		{"a public DNS resolver", "8.8.8.8", 53},
	} {
		if direct(d.host, d.port) {
			t.Errorf("%s (%s:%d) was directly reachable; the capsule has a route that bypasses the relay",
				d.name, d.host, d.port)
			dumpSandboxDiag(ctx, t, dock, capsuleID, leaseID)
		}
	}

	// DNS resolves — through the gateway's relay, the only resolver the
	// capsule can reach.
	if code, out, err := dock.Exec(ctx, string(capsuleID), []string{"getent", "hosts", "example.com"}); err != nil || code != 0 {
		t.Errorf("DNS through the gateway failed: exit %d, %v, %s", code, err, out)
		dumpSandboxDiag(ctx, t, dock, capsuleID, leaseID)
	}

	// Public egress works through the relay: this is the path a job's
	// checkout, package install and image pull take.
	if ok, out := relayed("https://example.com"); !ok {
		t.Errorf("relayed public egress failed: %s", out)
		dumpSandboxDiag(ctx, t, dock, capsuleID, leaseID)
	}

	// The relay applies the policy to what it resolves, so a name that
	// answers with a denied address is refused — the DNS rebinding
	// case, and the reason names are not the unit of policy.
	for _, denied := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/",
		"http://10.0.0.1/",
		"http://" + firstHost(uplinkSubnet) + "/",
	} {
		ok, out := relayed(denied)
		if ok && !strings.HasPrefix(out, "403") {
			t.Errorf("relay reached %s (status %s); the address policy leaked", denied, out)
		}
	}

	// Losing the gateway must not degrade into open egress: with the
	// relay gone the capsule has nothing at all.
	short := leaseID
	if len(short) > 12 {
		short = short[:12]
	}
	gwID, err := dock.OwnedIDByName(ctx, engine.KindContainer, "runpool-"+string(engine.RoleGateway)+"-"+string(short), "contract", assignment.LeaseID(leaseID))
	if err != nil || gwID == "" {
		t.Fatalf("resolve gateway: %q, %v", gwID, err)
	}
	if err := dock.RemoveContainer(ctx, gwID); err != nil {
		t.Fatalf("remove gateway: %v", err)
	}
	if ok, _ := relayed("https://example.com"); ok {
		t.Error("egress survived the gateway's removal")
	}
	if direct("1.1.1.1", 443) {
		t.Error("the capsule reached the internet directly after the gateway was removed")
	}
}

// dumpSandboxDiag prints the sandbox's live state when an assertion
// fails, so a failure is diagnosed from the actual routes and rules
// rather than guessed at.
func dumpSandboxDiag(ctx context.Context, t *testing.T, dock *docker.Client,
	capsuleID assignment.RuntimeID, leaseID assignment.LeaseID) {
	t.Helper()
	show := func(container, label string, cmd ...string) {
		code, out, err := dock.Exec(ctx, container, cmd)
		t.Logf("--- %s (exit %d, err %v) ---\n%s", label, code, err, out)
	}
	show(string(capsuleID), "capsule: ip route", "ip", "route")
	show(string(capsuleID), "capsule: resolv.conf", "cat", "/etc/resolv.conf")
	show(string(capsuleID), "capsule: proxy env", "bash", "-c", "env | grep -i proxy | sort")

	short := leaseID
	if len(short) > 12 {
		short = short[:12]
	}
	gwID, err := dock.OwnedIDByName(ctx, engine.KindContainer, "runpool-"+string(engine.RoleGateway)+"-"+string(short), "contract", assignment.LeaseID(leaseID))
	if err != nil || gwID == "" {
		t.Logf("could not resolve gateway for diagnostics: %v", err)
		return
	}
	show(gwID, "gateway: addresses", "ip", "-o", "addr")
	show(gwID, "gateway: iptables-save", "iptables-save")
	logs, _ := dock.TailLogs(ctx, gwID, 50)
	t.Logf("--- gateway logs ---\n%s", logs)
}

// firstHost returns the first host address of a CIDR (network + 1),
// which on the uplink is where the host's own bridge sits — a
// destination the capsule must never reach.
func firstHost(cidr string) string {
	base, _, _ := strings.Cut(cidr, "/")
	octets := strings.Split(base, ".")
	if len(octets) != 4 {
		return base
	}
	return fmt.Sprintf("%s.%s.%s.1", octets[0], octets[1], octets[2])
}
