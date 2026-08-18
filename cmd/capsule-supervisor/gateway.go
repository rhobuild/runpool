package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rhobuild/runpool/internal/gateway"
)

// The gateway runs from this same binary in a different container, so
// the capsule image carries one executable. The behaviour lives in
// internal/gateway, where it can be tested without a container; what
// remains here is the process shell: signals, the control directory,
// and the supervisor's state protocol.

const policyEnv = "RUNPOOL_GATEWAY_POLICY"

func runGateway(log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	err := gateway.Run(ctx, gateway.Config{
		Policy:     os.Getenv(policyEnv),
		ControlDir: controlDir,
		Log:        log,
		SetState:   setState,
	})
	if err != nil {
		log.Error("gateway failed", "error", err)
		setState("failed:" + err.Error())
		return 1
	}
	return 0
}

// runGatewayReload installs a new allow/deny set on a live gateway,
// reading it from stdin.
func runGatewayReload() int {
	if err := gateway.Reload(controlDir, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "reload:", err)
		return 1
	}
	return 0
}

// runGatewayDenyAll closes a live gateway's policy to everything. The
// controller calls it when it cannot prove the policy in force still
// describes the environment — a gateway relaying under a policy that
// predates a changed network is the failure this replaces.
func runGatewayDenyAll() int {
	if err := gateway.DenyAll(controlDir); err != nil {
		fmt.Fprintln(os.Stderr, "deny-all:", err)
		return 1
	}
	return 0
}
