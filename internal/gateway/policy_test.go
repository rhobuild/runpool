package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPolicyGenerationAdvancesOnlyOnChange: the relay drops pooled
// connections when the policy moves, because a connection authorised under
// the previous one is reused without ever reaching the dialer where the
// address check lives. That hinges on the counter advancing exactly when a
// new policy takes effect — never on an unchanged re-read, or every request
// would tear down the pool.
func TestPolicyGenerationAdvancesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.0.0.0/8")

	s := &PolicyStore{Path: path}
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
	first := s.Generation()
	if first == 0 {
		t.Fatal("generation stayed zero after the first policy was installed")
	}

	// An unchanged file is served from cache and must not advance.
	for range 3 {
		if _, err := s.Current(); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Generation(); got != first {
		t.Errorf("generation moved to %d on an unchanged policy; the pool would be dropped every request", got)
	}

	// A restriction that actually changed must advance it.
	time.Sleep(10 * time.Millisecond) // distinct mtime
	write("192.168.0.0/16")
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
	if got := s.Generation(); got == first {
		t.Error("generation did not move on a new policy; connections pooled under the old one would keep it")
	}
}

// TestRelayNoticesAPolicyChangeWithoutDialing is the regression this fix
// first got wrong. The generation only advances when Current() re-reads the
// file, and Current() is otherwise reached only from dial() — which a
// request served from the connection pool never calls. Checking the
// generation without reading the policy therefore could not detect a change
// in precisely the case the pool-expiry exists for.
func TestRelayNoticesAPolicyChangeWithoutDialing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.0.0.0/8")

	r := &Relay{Policy: &PolicyStore{Path: path}, Log: discardLogger()}
	r.roundTripper() // the transport must exist before the pool can be dropped

	// Two observations settle the baseline; no dial happens in either.
	r.expireStaleConnections()
	r.expireStaleConnections()
	baseline := r.policyGen.Load()
	if baseline == 0 {
		t.Fatal("the relay never read the policy; a pooled request would keep the old one forever")
	}

	r.expireStaleConnections()
	if got := r.policyGen.Load(); got != baseline {
		t.Errorf("generation moved to %d with an unchanged policy; the pool would be dropped every request", got)
	}

	time.Sleep(10 * time.Millisecond) // distinct mtime
	write("192.168.0.0/16")
	r.expireStaleConnections()
	if got := r.policyGen.Load(); got == baseline {
		t.Error("the relay did not notice a new policy without dialing; pooled connections would keep the old one")
	}
}
