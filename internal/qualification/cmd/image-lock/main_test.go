package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rhobuild/runpool/internal/imagelock"
)

const (
	locked  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	drifted = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// registry answers as `docker buildx imagetools inspect --format` would,
// which is the seam this gate is decidable behind. What it cannot stand in
// for is the template itself: the skipping of attestation manifests is done
// by `platformTemplate`, so only a real index exercises that.
func registry(digest, platforms string, err error) func(string, string) (string, error) {
	return func(_, format string) (string, error) {
		if err != nil {
			return "", err
		}
		if format == platformTemplate {
			return platforms, nil
		}
		return `"` + digest + `"`, nil
	}
}

func lockOf(digest string, platforms ...string) imagelock.Lock {
	return imagelock.Lock{
		Platforms: platforms,
		Images:    map[string]imagelock.Image{"runner": {Ref: "ghcr.io/example/runner:1", Digest: digest}},
	}
}

// TestTheGateFailsOnEveryDisagreementWithTheRegistry: this is the one check
// that looks at a registry rather than at another list, so a gate that
// returned success for any of these would leave the lock describing bytes
// nobody publishes while every other check stayed green.
func TestTheGateFailsOnEveryDisagreementWithTheRegistry(t *testing.T) {
	both := []string{"linux/amd64", "linux/arm64"}
	for name, tc := range map[string]struct {
		lock    imagelock.Lock
		inspect func(string, string) (string, error)
		wantErr bool
	}{
		"in step with the registry": {
			lock:    lockOf(locked, both...),
			inspect: registry(locked, "linux/amd64 linux/arm64", nil),
		},
		"the digest drifted under the tag": {
			lock:    lockOf(locked, both...),
			inspect: registry(drifted, "linux/amd64 linux/arm64", nil),
			wantErr: true,
		},
		"the registry could not be reached": {
			lock:    lockOf(locked, both...),
			inspect: registry("", "", errors.New("no route to host")),
			wantErr: true,
		},
		"a declared platform is not published": {
			lock:    lockOf(locked, both...),
			inspect: registry(locked, "linux/amd64", nil),
			wantErr: true,
		},
		"the entry names bytes no registry could serve": {
			lock:    lockOf("sha256:abc", both...),
			inspect: registry(locked, "linux/amd64 linux/arm64", nil),
			wantErr: true,
		},
		"the lock declares no image": {
			lock:    imagelock.Lock{Platforms: both},
			inspect: registry(locked, "linux/amd64 linux/arm64", nil),
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := verify(tc.lock, tc.inspect)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verify = %v; want error: %v", err, tc.wantErr)
			}
		})
	}
}

// The gate reports every entry rather than stopping at the first failure,
// because a maintainer resolving drift wants the whole picture and a second
// registry round trip to find the next problem is a slow way to get it.
func TestTheGateInspectsEveryEntryEvenAfterOneFails(t *testing.T) {
	lock := imagelock.Lock{
		Platforms: []string{"linux/amd64"},
		Images: map[string]imagelock.Image{
			"dind":   {Ref: "docker:29-dind", Digest: drifted},
			"runner": {Ref: "ghcr.io/example/runner:1", Digest: locked},
		},
	}
	asked := map[string]bool{}
	inspect := func(ref, format string) (string, error) {
		asked[ref] = true
		if format == platformTemplate {
			return "linux/amd64", nil
		}
		return fmt.Sprintf("%q", locked), nil
	}
	if err := verify(lock, inspect); err == nil {
		t.Fatal("verify succeeded with a drifted digest")
	}
	for _, ref := range []string{"docker:29-dind", "ghcr.io/example/runner:1"} {
		if !asked[ref] {
			t.Errorf("the gate never asked the registry about %q; one failing entry hid the rest", ref)
		}
	}
}
