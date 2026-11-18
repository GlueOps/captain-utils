// Command captain-utils upgrades and inspects a GlueOps captain from its
// captain repo. It replaces the codespace's bash/Python captain_utils one area
// at a time; see README.md.
package main

import (
	"os"

	"github.com/glueops/captain-utils/internal/cmd"
)

// version and commit are set at build time: -ldflags="-X main.version=v1.2.3 -X main.commit=abc".
var (
	version = "dev"
	commit  = "local"
)

func main() { os.Exit(cmd.Main(version, commit)) }
