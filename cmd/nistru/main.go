// Command nistru is a minimal terminal text editor.
// See https://github.com/savimcio/nistru for usage and architecture.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/savimcio/nistru/internal/editor"
)

func main() {
	path := flag.String("path", ".", "root directory for the file tree")
	flag.Parse()
	if err := editor.Run(*path); err != nil {
		fmt.Fprintf(os.Stderr, "nistru: %v\n", err)
		os.Exit(1)
	}
}
