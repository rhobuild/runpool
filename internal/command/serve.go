package command

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/config"
)

// The state mount point in the reference deployment; RUNPOOL_STATE_DIR
// overrides it for development hosts. Cache lanes are daemon-side
// volumes and need no directory of the controller's own.
const defaultStateDir = "/var/lib/runpool/state"

func stateDir() string { return dirOrDefault("RUNPOOL_STATE_DIR", defaultStateDir) }

func dirOrDefault(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func runServe(build BuildInfo) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A cancelled context is the signal that asked for the shutdown, so
	// a clean drain is a success however it was triggered.
	err = app.Serve(ctx, cfg, app.Options{
		Version:      build.Version,
		CapsuleImage: build.CapsuleImage,
		StateDir:     stateDir(),
		Environ:      os.Getenv,
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
