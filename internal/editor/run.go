package editor

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

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
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
