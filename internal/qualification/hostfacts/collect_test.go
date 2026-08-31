package hostfacts

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
)

func TestCollectorBuildsTypedFactsFromItsSources(t *testing.T) {
	fixedTime := time.Date(2026, time.August, 30, 18, 45, 0, 0, time.FixedZone("test", -4*60*60))
	c := collector{
		readFile: func(string) ([]byte, error) {
			return []byte("ID=debian\nVERSION_ID=\"13\"\nVERSION_CODENAME='trixie'\n"), nil
		},
		run: func(_ context.Context, name string, arguments ...string) (string, error) {
			command := strings.Join(append([]string{name}, arguments...), " ")
			switch command {
			case "uname -r":
				return "6.12.48+deb13-amd64\n", nil
			case "findmnt -no FSTYPE --target /var/lib/docker":
				return "ext4\n", nil
			case "docker buildx version":
				return "github.com/docker/buildx v0.32.0 deadbeef\n", nil
			case "docker compose version --short":
				return "2.42.0\n", nil
			case "iptables --version":
				return "iptables v1.8.11 (nf_tables)\n", nil
			case "nft --version":
				return "nftables v1.1.1 (Commodore Bullmoose)\n", nil
			default:
				return "", errors.New("not installed")
			}
		},
		docker: func(context.Context) (dockerFacts, error) {
			return dockerFacts{
				engine: "29.7.2", api: "1.53", cgroupVersion: "2", cgroupDriver: "systemd",
				storageDriver: "overlayfs", dockerRoot: "/var/lib/docker", containerd: "2.2.0",
				runc: "1.4.2",
			}, nil
		},
		now:        func() time.Time { return fixedTime },
		goarch:     "amd64",
		osRelease: "/test/os-release",
	}

	document, err := c.collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rootless := false
	want := Document{
		Facts: platform.Facts{
			OS: "debian", OSVersion: "13", OSCodename: "trixie", Arch: "amd64",
			Kernel: "6.12.48+deb13-amd64", Engine: "29.7.2", API: "1.53",
			CgroupVersion: "2", CgroupDriver: "systemd", StorageDriver: "overlayfs",
			BackingFilesystem: "ext4", Rootless: &rootless, Containerd: "2.2.0", Runc: "1.4.2",
			Buildx: "v0.32.0", Compose: "2.42.0", IPTables: "iptables v1.8.11 (nf_tables)",
			NFTables: "nftables v1.1.1 (Commodore Bullmoose)",
		},
		CollectedAt: "2026-08-30T22:45:00Z",
	}
	if !reflect.DeepEqual(document, want) {
		t.Fatalf("document = %#v; want %#v", document, want)
	}
}

func TestCollectorFailsWhenTheBackingFilesystemCannotBeObserved(t *testing.T) {
	c := collector{
		readFile: func(string) ([]byte, error) { return []byte("ID=debian\n"), nil },
		run: func(_ context.Context, name string, arguments ...string) (string, error) {
			if name == "uname" {
				return "kernel", nil
			}
			return "", errors.New("findmnt is unavailable")
		},
		docker: func(context.Context) (dockerFacts, error) {
			return dockerFacts{dockerRoot: "/var/lib/docker"}, nil
		},
		now:        time.Now,
		goarch:     "amd64",
		osRelease: "/test/os-release",
	}

	_, err := c.collect(t.Context())
	if err == nil || !strings.Contains(err.Error(), "backing filesystem") {
		t.Fatalf("error = %v; want a backing-filesystem collection failure", err)
	}
}

func TestParseOSReleaseHandlesTheSpecifiedQuotingForms(t *testing.T) {
	values, err := parseOSRelease(strings.NewReader(
		"ID=debian\nVERSION_ID='13'\nVERSION_CODENAME=\"tri\\\"xie\"\nNAME=\"a\\q\"\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"ID": "debian", "VERSION_ID": "13", "VERSION_CODENAME": `tri"xie`, "NAME": `a\q`,
	} {
		if got := values[key]; got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}
}
