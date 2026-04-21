package editor

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the editor's Bubble Tea program rooted at path and
// guarantees a final autosave flush on exit. It is the single entry
// point consumed by cmd/nistru.
func Run(path string) error {
	m, err := NewModel(path)
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
