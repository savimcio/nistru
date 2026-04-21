package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	path := flag.String("path", ".", "root directory for the file tree")
	flag.Parse()

	m, err := NewModel(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nistru: %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nistru: %v\n", err)
		os.Exit(1)
	}

	// Final synchronous flush in case the event loop tore down before the
	// debounced tick could land. flushNow is a no-op when not dirty.
	if fm, ok := finalModel.(*Model); ok {
		if err := fm.flushNow(); err != nil {
			fmt.Fprintf(os.Stderr, "nistru: final flush failed: %v\n", err)
			os.Exit(1)
		}
	}
}
