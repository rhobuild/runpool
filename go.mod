module github.com/rhobuild/runpool

go 1.26.6

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.21 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/rhysd/actionlint v1.7.12 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.3 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/tools v0.48.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

// Development tools are pinned here and run with `go tool`, so a gate
// never resolves a scanner or linter at run time — a moving @latest
// makes a security gate unreproducible.
tool (
	github.com/rhysd/actionlint/cmd/actionlint
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
