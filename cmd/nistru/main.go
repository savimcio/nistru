// Command nistru is a minimal terminal text editor.
// See https://github.com/savimcio/nistru for usage and architecture.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/internal/editor"
	"github.com/savimcio/nistru/internal/plugins/autoupdate"
)

// Version is the build version. Defaults to "dev"; override via
// -ldflags "-X main.Version=..." at build time. The autoupdate plugin
// reads this via SetVersion so a ldflags-stamped local build and the
// update checker agree on what "current" means.
var Version = "dev"

func main() {
	path := flag.String("path", ".", "root directory for the file tree")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}
	// Resolve the layered TOML config before the editor starts. Missing
	// files aren't errors; malformed files / bad values become warnings
	// surfaced on stderr so the user notices without losing the session.
	cfg, warnings, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nistru: config: %v\n", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "nistru: config: %s: %s\n", w.Source, w.Message)
	}
	// Pass the ldflags-stamped Version to the autoupdate plugin *before*
	// the editor starts. The plugin treats "dev" as "not injected" and
	// falls back to ReadBuildInfo for `go install @tag` installs.
	autoupdate.SetVersion(Version)
	if err := editor.Run(*path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "nistru: %v\n", err)
		os.Exit(1)
	}
}
