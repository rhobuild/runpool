package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestAReplacedFileIsNeverReadHalfWritten: a reader racing writers sees
// one whole payload or the other, never a partial one and never nothing.
//
// Both callers have a reader in another process on its own schedule,
// with no lock and no handshake: the relay reads the egress policy while
// an install replaces it, and the controller `cat`s the capsule's
// protocol declaration and execs it for its state while the supervisor
// writes both. A write that truncates first hands those readers an empty
// file from a call that succeeded, and an empty answer is not "not yet"
// to any of them — it is a policy that does not parse, a protocol
// version nobody speaks, or a state nothing recognizes.
func TestAReplacedFileIsNeverReadHalfWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	// Payloads of very different lengths: a tear between two equal ones
	// can look intact, and it is the tail of the longer that makes a
	// partial write visible.
	payloads := [][]byte{[]byte("2"), []byte(strings.Repeat("9", 4096))}
	if err := Replace(path, payloads[0], 0o600, -1, -1); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	// The reader is joined rather than abandoned: a tear found on the
	// last write must still be reported, and a finding sent after the
	// check would simply be lost.
	watched := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				// A rename never leaves the target absent, so this is a
				// finding rather than something to poll past: a writer
				// that removed and recreated the file would show up here
				// and nowhere else.
				select {
				case bad <- "unreadable: " + err.Error():
				default:
				}
				return
			}
			whole := false
			for _, want := range payloads {
				if string(raw) == string(want) {
					whole = true
				}
			}
			if !whole {
				select {
				case bad <- fmt.Sprintf("%d bytes: %.32q", len(raw), raw):
				default:
				}
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				if err := Replace(path, payloads[i%len(payloads)], 0o600, -1, -1); err != nil {
					t.Errorf("replace: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-watched

	select {
	case raw := <-bad:
		t.Fatalf("a reader observed a file that is neither payload — %s", raw)
	default:
	}
}

// TestAFailedReplaceLeavesNothingBehind: the temporary file does not
// outlive a failure, and a successful one does not remove a name it no
// longer owns.
func TestAFailedReplaceLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := Replace(path, []byte("in force"), 0o600, -1, -1); err != nil {
		t.Fatal(err)
	}
	// An owner this process cannot grant: the chown fails after the
	// temporary file exists and has been written.
	if err := Replace(path, []byte("next"), 0o600, 0, 0); err == nil && os.Geteuid() != 0 {
		t.Fatal("chowning to root succeeded as an unprivileged user; this test asserted nothing")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "doc" {
			t.Errorf("a failed replace left %q behind", e.Name())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && string(raw) != "in force" {
		t.Errorf("the file reads %q; a replace that failed must not have changed it", raw)
	}
}
