package gateway

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/rhobuild/runpool/internal/egress"
)

// ApplyFirewall installs the filter ruleset in one restore call, which
// the kernel applies atomically: there is no instant in which half a
// policy is in force. The nat table is left alone on purpose — the
// gateway routes nothing, and flushing nat would delete the daemon's
// own rules for the embedded resolver this container resolves through.
func ApplyFirewall(p egress.Policy, l Legs) error {
	if err := restore("iptables-restore", p.RenderIPTables(l.InternalIf, egress.ProxyPort)); err != nil {
		return err
	}
	return restore("ip6tables-restore", egress.RenderIP6Tables())
}

func restore(tool, rules string) error {
	cmd := exec.Command(tool)
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", tool, err, out)
	}
	return nil
}
