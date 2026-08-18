// Command runpool is the Runpool controller and operations CLI.
package main

import (
	"os"

	"github.com/rhobuild/runpool/internal/command"
)

// version is stamped by the release build; "dev" identifies local builds.
var version = "dev"

// capsuleImage is the immutable capsule artifact paired with this
// controller. Development builds use a local tag; release builds stamp a
// digest-qualified registry reference after that image is release-qualified.
var capsuleImage = "runpool-capsule:dev"

func main() {
	os.Exit(command.Run(os.Args[1:], command.BuildInfo{
		Version:      version,
		CapsuleImage: capsuleImage,
	}))
}
