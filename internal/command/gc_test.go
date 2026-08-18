package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/store"
)

func capture() (IO, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	return IO{In: strings.NewReader(""), Out: &out, Err: &errBuf}, &out, &errBuf
}

// TestReportLeaseHistorySaysWhatItWouldDo. Retention deletes an
// operator's records without being asked, so this line is the only place
// they see it happening. Three things it has to get right: zero says it
// keeps everything, a dry run promises rather than acts, and a count
// that reached the per-run limit says so — an operator who reads the
// same number from the preview and the apply otherwise concludes the
// backlog is gone.
func TestReportLeaseHistorySaysWhatItWouldDo(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{}
	config.ApplyDefaults(cfg)

	streams, out, _ := capture()
	if err := reportLeaseHistory(t.Context(), streams, st, cfg, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would forget 0 record(s)") {
		t.Errorf("a dry run over an empty store said %q; it must promise, and say how much", out.String())
	}
	if strings.Contains(out.String(), "limit") {
		t.Errorf("a count nowhere near the per-run limit claimed to be capped: %q", out.String())
	}

	keep := config.Duration(0)
	cfg.Retention.LeaseHistory = &keep
	streams, out, _ = capture()
	if err := reportLeaseHistory(t.Context(), streams, st, cfg, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kept indefinitely") {
		t.Errorf("a zero window said %q; the operator asked for every record to be kept "+
			"and has to be told that is what is happening", out.String())
	}
}
