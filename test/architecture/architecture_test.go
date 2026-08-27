// Package architecture enforces the dependency direction the design
// rests on: the domain never points at a provider. The rule is a test
// rather than a convention because conventions drift one convenient
// import at a time, and each one quietly couples a schema, a state
// machine or a recovery path to one vendor's API.
package architecture

import (
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	module  = "github.com/rhobuild/runpool"
	adapter = module + "/internal/platform/githubactions"
	// upstream is the provider SDK itself; only the adapter and its
	// contract suite may see it.
	upstream = "github.com/actions/scaleset"
	// engineAdapter is the Moby adapter, and engineSDKs are the modules
	// only it may see.
	engineAdapter = module + "/internal/engine/docker"
)

// engineSDKs are the container-engine libraries. Their types describe a
// daemon's API, and a domain package holding one is a domain package
// that has learned which daemon it is talking to.
var engineSDKs = []string{
	"github.com/moby/",
	"github.com/containerd/errdefs",
}

// corePackages are the provider-neutral packages. None of them may
// depend on the GitHub Actions adapter or the upstream SDK, directly or
// transitively. internal/app and internal/command are deliberately
// absent: they are the wiring layers, and injecting the adapter is
// their job.
var corePackages = []string{
	module + "/internal/assignment",
	module + "/internal/store",
	module + "/internal/lease",
	module + "/internal/gateway",
	module + "/internal/capsule",
	module + "/internal/cache",
	module + "/internal/allocator",
	module + "/internal/disk",
	module + "/internal/egress",
	module + "/internal/credential",
	module + "/internal/platform",
	module + "/internal/engine",
	module + "/internal/netsandbox",
	module + "/internal/engine/docker",
	module + "/internal/config",
	module + "/internal/doctor",
}

// narrowDeps pins the internal packages each listed package may import
// directly. A dependency is not only weight, it is what a package is
// allowed to know — and both are lost the same way, one convenient
// import at a time. Listing the set exactly, rather than forbidding
// named packages, is what makes the failure say what was added.
//
// Direct imports, not the closure: the closure is checked separately
// where transitive weight is the point.
var narrowDeps = map[string][]string{
	// The four packages every other one stands on, pinned at what they
	// hold today: the shared vocabulary, the capsule wire format, the
	// atomic write, and the egress policy both the validator and the
	// enforcer read. They are not narrow because an edge was removed: they
	// are narrow because nothing has been added, and nothing states that.
	// An edge from any of them compiles, creates no cycle, and trips no
	// other rule here -- so the vocabulary every layer shares would learn
	// configuration, or the clientless port would learn persistence, and
	// the suite would say the architecture still holds.
	module + "/internal/assignment":          {},
	module + "/internal/capsule/protocol":    {},
	module + "/internal/platform/atomicfile": {},
	module + "/internal/egress":              {},
	// The capsule supervisor is PID 1 beside the job, and the gateway it
	// starts is the process that decides what that job reaches. What it
	// links is what an escape finds already loaded: the engine adapter
	// would put the host's Docker socket vocabulary there, the store its
	// persistence, the provider adapter the credential paths. None of
	// that is needed to boot a runner and report its state, and none of
	// it trips another rule here -- the rosters below cover internal
	// packages, and a command is not one.
	//
	// The controller is the other half and is pinned by the release
	// instead: it carries the capsule's digest stamped into it and
	// refuses any other, which is a pairing a single binary could not
	// express.
	module + "/cmd/capsule-supervisor": {
		module + "/internal/capsule/protocol",
		module + "/internal/gateway",
		module + "/internal/platform/atomicfile",
	},
	// The port names what a container engine does and holds no client.
	module + "/internal/engine": {module + "/internal/assignment"},
	// And its adapter translates; an adapter that learned the store would
	// put persistence behind the port every consumer thinks is a daemon.
	module + "/internal/engine/docker": {
		module + "/internal/assignment",
		module + "/internal/engine",
	},
	// The egress policy engine: discovery, the snapshot a launch is cut
	// from, and the fail-closed rule that a restriction must reach every
	// gateway before it is recorded as in force. It lived in the
	// composition root, which is exempt from every rule in this file --
	// so the one package that decides what a capsule can reach was the
	// one package free to import anything.
	module + "/internal/netsandbox": {
		module + "/internal/assignment",
		module + "/internal/capsule",
		module + "/internal/capsule/protocol",
		module + "/internal/config",
		module + "/internal/egress",
		module + "/internal/engine",
	},
	// config validates egress prefixes against the baseline deny set.
	// Restating those prefixes here instead would let the validator and
	// the enforcer disagree about what "private" means, which is a
	// security bug rather than a duplication.
	module + "/internal/config": {module + "/internal/egress"},
	// The lease machine settles attempts. It must never need a container
	// runtime to do it — cleanup that depends on one stops working
	// exactly when the runtime is the thing that failed.
	module + "/internal/lease": {
		module + "/internal/assignment",
		module + "/internal/store",
	},
	// doctor validates a host before anything serves, so what it may
	// know is the contract's vocabulary and the probes it drives — never
	// the store, the lease machine or an adapter: a preflight that
	// imports the machinery it is clearing would validate the host with
	// the thing that needs the host validated.
	module + "/internal/doctor": {
		module + "/internal/capsule",
		module + "/internal/config",
		module + "/internal/credential",
		module + "/internal/engine",
		module + "/internal/platform",
		// The adapter, for one field. Options.Docker is the concrete
		// client because a nil one has to stay a nil interface: wrapped,
		// it is a non-nil value holding nothing, and every guard below
		// it passes before the next line calls a method on nothing. The
		// conversion happens once, inside Run. Taking an interface here
		// would move that trap out to each caller that may have failed
		// to connect.
		module + "/internal/engine/docker",
	},
	// capsule talks to the daemon; it must not also import the gateway,
	// which is a separate process with its own lifecycle. The protocol
	// leaf is the values the supervisor and the launcher must agree on —
	// one declaration both sides import is what makes their drift
	// unrepresentable, so the edge is the point rather than a
	// convenience.
	module + "/internal/capsule": {
		module + "/internal/assignment",
		module + "/internal/capsule/protocol",
		module + "/internal/config",
		module + "/internal/egress",
		module + "/internal/engine",
	},
	// qualification assembles the record a release is authorized against.
	// It reads evidence files and the reviewed reference, and that is the
	// whole of what it may know: an edge to the store, the lease machine
	// or the wiring layers would make the gate that authorizes a release
	// depend on the code being released, which is a gate measuring
	// itself. Assemble takes the reference as a parameter rather than
	// loading it, and this is what keeps that shape.
	module + "/internal/qualification": {module + "/internal/platform"},
	// The gateway is the one process a capsule's traffic passes through,
	// and it runs from the capsule image rather than the controller
	// binary. What it may know is the policy vocabulary it enforces, the
	// control protocol it answers on, and the file replacement its
	// installs need — never the store, the lease machine or an adapter.
	// A relay that could reach the books is a relay that can be made to
	// answer a question instead of forwarding a packet.
	module + "/internal/gateway": {
		module + "/internal/capsule/protocol",
		module + "/internal/egress",
		module + "/internal/platform/atomicfile",
	},
}

func deps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		set[strings.TrimSpace(line)] = true
	}
	return set
}

// TestCoreDoesNotDependOnTheProviderAdapter walks the whole module, the
// way the engine-SDK rule does, because the rule it enforces is about
// every package that is not the wiring: iterated over corePackages
// alone, a new package escaped it by not being listed — the list said
// which packages were checked, not which were allowed, and nothing
// failed when those diverged. The wiring layers are the exemption, named
// here with the reason the corePackages comment already gives: injecting
// the adapter is their job. corePackages stays as the roster the
// narrow-dependency rules run over.
func TestCoreDoesNotDependOnTheProviderAdapter(t *testing.T) {
	wiring := map[string]bool{
		module + "/internal/app":     true,
		module + "/internal/command": true,
		module + "/cmd/runpool":      true,
		adapter:                      true,
	}
	graph := importGraph(t)
	for importer, imports := range graph {
		if wiring[importer] || strings.HasPrefix(importer, adapter+"/") ||
			strings.HasPrefix(importer, module+"/test/") {
			continue
		}
		for _, imported := range imports {
			if imported == adapter {
				t.Errorf("%s imports %s; the domain must stay provider-neutral — "+
					"translate at the adapter and hand the domain opaque keys", importer, adapter)
			}
			if imported == upstream || strings.HasPrefix(imported, upstream+"/") {
				t.Errorf("%s imports %s; only the adapter, the wiring and their "+
					"contract suites may see the provider SDK", importer, imported)
			}
		}
	}
	// Direct imports catch the edge someone adds; the closure is what
	// catches it arriving through a third package. The roster's packages
	// keep the transitive check they always had.
	for _, pkg := range corePackages {
		t.Run(strings.TrimPrefix(pkg, module+"/"), func(t *testing.T) {
			d := deps(t, pkg)
			if d[adapter] {
				t.Errorf("%s depends on %s transitively", pkg, adapter)
			}
			if d[upstream] {
				t.Errorf("%s depends on %s transitively", pkg, upstream)
			}
		})
	}
}

// TestAdapterDependsOnTheDomainNotTheReverse pins the arrow's direction:
// the adapter translating into assignment types is correct, and the
// domain importing the adapter would invert it.
func TestAdapterDependsOnTheDomainNotTheReverse(t *testing.T) {
	if !deps(t, adapter)[module+"/internal/assignment"] {
		t.Errorf("%s no longer depends on internal/assignment; the adapter must "+
			"translate into the domain's neutral types rather than exporting its own", adapter)
	}
}

// importGraph is every package in the module with its direct imports.
//
// The pattern is the module's own, never "./...": a test binary runs in
// its own source directory, where "./..." matches one package — this
// file's. A walk written that way reports nothing and passes, which is
// the shape a rule about the whole tree must not have.
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f",
		`{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`,
		module+"/...")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		graph[fields[0]] = fields[1:]
	}
	if len(graph) < 2 {
		t.Fatalf("the module lists %d packages, so this proves nothing", len(graph))
	}
	return graph
}

// TestGeneratedPersistenceStaysInsideTheStore: sqlc rows are not domain
// types, and a coordinator that imports them couples its decisions to a
// table layout. Only internal/store may see the generated package;
// everything else talks to repositories that translate raw outcomes
// into domain errors.
func TestGeneratedPersistenceStaysInsideTheStore(t *testing.T) {
	generated := module + "/internal/store/sqlitedb"
	for importer, imports := range importGraph(t) {
		if importer == module+"/internal/store" || importer == generated {
			continue
		}
		for _, imported := range imports {
			if imported == generated {
				t.Errorf("%s imports %s; generated persistence is internal to the store package", importer, generated)
			}
		}
	}
}

// TestTheEngineSDKStaysInsideItsAdapter: the decision is written down
// and nothing enforced it.
//
// The Docker API client ADR says it plainly -- keep all daemon
// operations behind the Moby adapter; no domain package imports
// Moby types -- and the tree obeys it. What was missing is the thing
// that keeps obeying it: the architecture rules here covered the
// provider SDK and said nothing about the engine's, so the first import
// that crossed the line would have compiled, passed, and moved the
// boundary by one convenient edit.
func TestTheEngineSDKStaysInsideItsAdapter(t *testing.T) {
	for importer, imports := range importGraph(t) {
		if importer == engineAdapter {
			continue
		}
		for _, imported := range imports {
			for _, sdk := range engineSDKs {
				if !strings.HasPrefix(imported, sdk) {
					continue
				}
				t.Errorf("%s imports %s; only %s may see a container engine's own types, "+
					"and what leaves it is Runpool's vocabulary rather than a daemon's",
					importer, imported, engineAdapter)
			}
		}
	}
}

// TestReleaseToolingStaysOutOfTheProduct: the release gate is not part
// of what it gates.
//
// Nothing under internal/qualification ships. It has no place in either
// binary, and an import of it from a runtime package would link the
// vocabulary of the release gate into the thing being released —
// quietly, because it compiles. A directory name enforces none of that;
// this does.
func TestReleaseToolingStaysOutOfTheProduct(t *testing.T) {
	tooling := module + "/internal/qualification"
	for importer, imports := range importGraph(t) {
		if importer == tooling || strings.HasPrefix(importer, tooling+"/") {
			continue
		}
		for _, imported := range imports {
			if imported == tooling {
				t.Errorf("%s imports %s; the release gate must not link into the product it gates",
					importer, tooling)
			}
		}
	}
}

// directImports lists a package's own import statements, which is a
// different question from `go list -deps` — that returns the closure, and
// the closure cannot say which edge someone added.
func directImports(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var internal []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if imported := strings.TrimSpace(line); strings.HasPrefix(imported, module+"/internal/") {
			internal = append(internal, imported)
		}
	}
	sort.Strings(internal)
	return internal
}

// TestNarrowPackagesStayNarrow pins the dependency sets that were paid
// for. Each of these packages had an edge removed deliberately, and an
// exact list is the only form of that rule which survives: a "must not
// import X" rule protects one edge and says nothing about the next one.
func TestNarrowPackagesStayNarrow(t *testing.T) {
	for pkg, allowed := range narrowDeps {
		t.Run(strings.TrimPrefix(pkg, module+"/"), func(t *testing.T) {
			want := append([]string(nil), allowed...)
			sort.Strings(want)
			got := directImports(t, pkg)
			if !slices.Equal(got, want) {
				t.Errorf("internal imports of %s changed.\n got: %v\nwant: %v\n\n"+
					"If the new edge is intended, widen narrowDeps and say why in its comment; "+
					"if it is a convenience, find the value that belongs in a leaf both sides already import.",
					pkg, got, want)
			}
		})
	}
}

// TestTheLeaseMachineLinksNoContainerRuntime is the transitive half. The
// direct-import rule above would still pass if lease imported something
// that itself dragged the Docker client in, and the weight is the whole
// reason that edge was removed: the lease machine settles attempts from
// evidence, and it must stay compilable and testable without a runtime.
func TestTheLeaseMachineLinksNoContainerRuntime(t *testing.T) {
	const runtimePkg = module + "/internal/engine/docker"
	if deps(t, module+"/internal/lease")[runtimePkg] {
		t.Errorf("internal/lease reaches %s transitively; cleanup that depends on a "+
			"container runtime stops working exactly when the runtime is what failed", runtimePkg)
	}
}
