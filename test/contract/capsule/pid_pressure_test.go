package capsulecontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/config"
)

const (
	pidPressureHelperEnvironment = "RUNPOOL_PID_PRESSURE_HELPER"
	pidPressureDonePath          = "/tmp/runpool-pid-pressure.done"
	pidPressureLogPath           = "/tmp/runpool-pid-pressure.log"
	pidPressureBinaryPath        = "/tmp/runpool-pid-pressure"
)

type pidPressureEvidence struct {
	LimitReached bool  `json:"limit_reached"`
	Spawned      int   `json:"spawned"`
	Saturated    int64 `json:"saturated"`
	Recovered    int64 `json:"recovered"`
}

// TestMain lets the already-built contract binary act as a Linux-only pressure
// helper inside the gateway. Normal suite execution never exposes an extra test
// case or skip to qualification evidence.
func TestMain(m *testing.M) {
	if os.Getenv(pidPressureHelperEnvironment) == "1" {
		os.Exit(runPIDPressureHelper())
	}
	os.Exit(m.Run())
}

func runPIDPressureHelper() int {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open null device:", err)
		return 1
	}
	defer devNull.Close()

	children := make([]*os.Process, 0, config.GatewayReservePIDs)
	cleanup := func() {
		for _, child := range children {
			_ = child.Kill()
		}
		for _, child := range children {
			_, _ = child.Wait()
		}
	}
	defer cleanup()

	// Twice the configured reserve is a vacuity guard: reaching it would mean
	// the cgroup did not enforce the limit this helper was launched to measure.
	limitReached := false
	for len(children) < config.GatewayReservePIDs*2 {
		child, err := os.StartProcess("/bin/sleep", []string{"sleep", "300"}, &os.ProcAttr{
			Files: []*os.File{devNull, devNull, devNull},
		})
		if err == nil {
			children = append(children, child)
			continue
		}
		if errors.Is(err, syscall.EAGAIN) {
			limitReached = true
			break
		}
		fmt.Fprintln(os.Stderr, "start pressure process:", err)
		return 1
	}
	if !limitReached {
		fmt.Fprintln(os.Stderr, "started", len(children), "processes without reaching the PID limit")
		return 1
	}
	saturated, err := readPIDCounter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "read saturated PID counter:", err)
		return 1
	}

	// Hold the boundary long enough for it to represent sustained pressure.
	// External execs are intentionally unnecessary because the cgroup may
	// correctly reject them while it is full.
	time.Sleep(2 * time.Second)

	spawned := len(children)
	cleanup()
	children = nil
	recovered, err := readPIDCounter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "read recovered PID counter:", err)
		return 1
	}
	evidence, err := json.Marshal(pidPressureEvidence{
		LimitReached: true,
		Spawned:      spawned,
		Saturated:    saturated,
		Recovered:    recovered,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode PID pressure evidence:", err)
		return 1
	}
	temporaryPath := pidPressureDonePath + ".tmp"
	if err := os.WriteFile(temporaryPath, evidence, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write PID pressure evidence:", err)
		return 1
	}
	if err := os.Rename(temporaryPath, pidPressureDonePath); err != nil {
		fmt.Fprintln(os.Stderr, "publish PID pressure evidence:", err)
		return 1
	}
	return 0
}

func readPIDCounter() (int64, error) {
	value, err := os.ReadFile("/sys/fs/cgroup/pids.current")
	if err != nil {
		return 0, err
	}
	counter, err := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse pids.current: %w", err)
	}
	return counter, nil
}
