package editor

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/savimcio/nistru/internal/config"
)

// Run starts the editor's Bubble Tea program rooted at path and
// guarantees a final autosave flush on exit. It is the single entry
// point consumed by cmd/nistru.
//
// A nil cfg is replaced with config.Defaults() so callers that haven't
// wired in the loader yet still get a working editor.
func Run(path string, cfg *config.Config) error {
	m, err := NewModel(path, cfg)
	if err != nil {
		return err
	}
	// AltScreen and MouseCellMotion used to be Program-level options; in
	// bubbletea v2 they move to declarative fields on the tea.View returned
	// from Model.View(). See Model.View in model.go.
	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(*Model); ok {
		if err := fm.flushNow(); err != nil {
			return fmt.Errorf("final flush failed: %w", err)
		}
	}
	return nil
}
