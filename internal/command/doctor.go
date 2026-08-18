package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/doctor"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

func runDoctor(streams IO, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A configuration error is itself a diagnosis: report it and check
	// the host anyway, since the operator needs both answers at once.
	cfg, cfgErr := config.Load(os.Getenv)
	dock, dockErr := docker.New(ctx)
	if dockErr == nil {
		defer dock.Close()
	}

	opts := doctor.Options{
		Config:   cfg,
		Docker:   dock,
		StateDir: stateDir(),
		Environ:  os.Getenv,
	}
	// Injecting the probe is what asks for the live credential checks, and
	// it is this layer's job: the doctor stays provider-neutral, and only
	// the composition root names an adapter.
	if cfg != nil {
		opts.NewCredentialProbe = newGitHubCredentialProbe
		opts.HostedDomain = githubactions.IsHostedDomain
	}
	report := doctor.Run(ctx, opts)

	if asJSON {
		enc := json.NewEncoder(streams.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprint(streams.Out, report)
	}
	// The report itself is the output; a failing check has already been
	// printed line by line, so the error carries the verdict rather than
	// repeating the findings.
	switch {
	case cfgErr != nil:
		return fmt.Errorf("configuration: %w", cfgErr)
	case !report.OK():
		return errors.New("host does not meet the runtime contract; see the failing checks above")
	}
	return nil
}

// runHealthcheck is the container health command. Liveness answers
// whether this process can still reach its own state; readiness adds
// the host contract. It is deliberately cheap and local: no GitHub call,
// so a broker outage never marks the controller unhealthy.
func runHealthcheck(streams IO, mode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch mode {
	case "liveness":
		// The state directory is the one resource whose loss makes the
		// controller unable to do anything correctly.
		if _, err := os.Stat(stateDir() + "/" + store.DatabaseFile); err != nil {
			return fmt.Errorf("state database unreachable: %w", err)
		}
		fmt.Fprintln(streams.Out, "ok")
		return nil
	case "readiness":
		cfg, err := config.Load(os.Getenv)
		if err != nil {
			return fmt.Errorf("configuration: %w", err)
		}
		dock, err := docker.New(ctx)
		if err != nil {
			return fmt.Errorf("docker: %w", err)
		}
		defer dock.Close()
		report := doctor.Run(ctx, doctor.Options{
			Config: cfg, Docker: dock, StateDir: stateDir(), Environ: os.Getenv,
		})
		if !report.OK() {
			fmt.Fprint(streams.Err, report)
			return errors.New("host does not meet the runtime contract")
		}
		fmt.Fprintln(streams.Out, "ok")
		return nil
	default:
		return usagef("--mode must be liveness or readiness, not %q", mode)
	}
}

// newGitHubCredentialProbe builds the doctor's provider probe for one
// target. The error path returns an explicit nil: handing back a typed
// nil pointer would give the doctor a non-nil interface holding nothing.
func newGitHubCredentialProbe(configURL string, secret credential.Secret) (doctor.CredentialProbe, error) {
	client, err := githubactions.NewClient(githubactions.ClientConfig{
		ConfigURL: configURL, Credential: secret, Version: "doctor",
	})
	if err != nil {
		return nil, err
	}
	return githubProbe{client: client}, nil
}

// githubProbe narrows the adapter's client to what the doctor declares.
// The adapter answers with its own scale set type, and the doctor stays
// provider-neutral by never naming one — so the translation belongs here,
// in the layer that already knows which adapter this is.
//
// The client is a named field, not embedded: embedding would promote the
// adapter's whole method set, and the value handed to a package that
// promises never to mutate durable product state would carry
// DeleteScaleSet and RemoveRunner along with it. The two forwards below
// are the probe's entire surface, and a test holds the type to that.
type githubProbe struct{ client *githubactions.Client }

func (p githubProbe) RunnerGroupID(ctx context.Context, group string) (int, error) {
	return p.client.RunnerGroupID(ctx, group)
}

func (p githubProbe) ScaleSetID(ctx context.Context, group, name string) (int, bool, error) {
	set, found, err := p.client.ScaleSetByName(ctx, group, name)
	if err != nil || !found {
		return 0, false, err
	}
	return set.ID, true, nil
}
