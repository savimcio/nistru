package editor

import (
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// saveTickMsg is emitted by the debounce timer. The model only honours it when
// gen matches the current saveGen — later edits invalidate earlier ticks.
type saveTickMsg struct {
	gen int
}

// changeTickMsg is emitted by the change-debounce timer. The model only
// honours it when gen matches the current changeGen — later edits invalidate
// earlier ticks. Fires independently of saveTickMsg so plugin DidChange
// notifications can be delivered on a shorter cadence than autosave.
type changeTickMsg struct {
	gen int
}

// forceSaveMsg is dispatched by the Ctrl+S binding and handled at the model
// level to trigger an immediate synchronous flush (also bumps saveGen so any
// in-flight debounced tick becomes a no-op).
type forceSaveMsg struct{}

// forceQuitMsg is dispatched by Ctrl+Q and handled at the model level to
// flush any pending writes and quit.
type forceQuitMsg struct{}

// openFileRequestMsg is an internal message used to schedule openFile via the
// Update mutation point. Plugins (and pane effects) emit plugin.OpenFile,
// which the model translates into this msg so the open happens inside the
// ordinary Update → openFile path.
type openFileRequestMsg struct {
	path string
}

// scheduleSave returns a tea.Cmd that fires a saveTickMsg after the provided
// debounce window. Later edits bump saveGen so this tick will no-op when it
// lands. The debounce duration is supplied by the caller (see
// config.Autosave.SaveDebounce) so the helper stays pure.
func scheduleSave(gen int, debounce time.Duration) tea.Cmd {
	return tea.Tick(debounce, func(time.Time) tea.Msg {
		return saveTickMsg{gen: gen}
	})
}

// scheduleChange returns a tea.Cmd that fires a changeTickMsg after the
// provided debounce window. Later edits bump changeGen so this tick will
// no-op when it lands. Shorter than scheduleSave in practice because plugin
// consumers (formatters, linters) want quicker feedback than the autosave
// cadence; the exact value lives in config.Autosave.ChangeDebounce.
func scheduleChange(gen int, debounce time.Duration) tea.Cmd {
	return tea.Tick(debounce, func(time.Time) tea.Msg {
		return changeTickMsg{gen: gen}
	})
}

// atomicWriteFile writes data to path via a sibling .tmp file followed by
// os.Rename, so a crash mid-write cannot leave a half-written file at path.
// The tmp file is created with the destination's existing permission bits
// when possible, falling back to 0644 for new files. On any failure the tmp
// file is removed and the error is surfaced.
//
// Not fsync'd — a power loss during the rename window may lose the latest
// save but will never leave a half-written file.
func atomicWriteFile(path string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
