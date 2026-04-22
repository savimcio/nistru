package editor

import (
	"github.com/savimcio/nistru/internal/config"
)

// editorOpts bundles the config-driven knobs newEditor consumes. A nil
// pointer means "use defaults" — which keeps hand-constructed test Models
// calling newEditor("", "") without a config handy. The fields default to
// the built-in keymap and enabled relative numbers when zero-valued.
//
// Note: Keymap is currently plumbed through for future use. The adapter's
// interceptKey uses a hardcoded Ctrl+Z/Y/X/C/V mapping; wiring the user's
// keymap into the adapter is a follow-up task (see the Keymap field below).
type editorOpts struct {
	Keymap          config.Keymap
	RelativeNumbers bool
}

// newEditor constructs a fresh Editor (backed by goeditorAdapter) seeded
// with the given content. The returned value satisfies the package-local
// Editor interface so the outer Model is decoupled from any specific
// library.
//
// opts (variadic, at most one) supplies the user's keymap and whether the
// editor renders relative line numbers. Passing no opts is equivalent to
// passing a nil pointer: defaults apply. Production callers pass the
// resolved config's fields; hand-constructed Models in tests omit it.
//
// This factory is called from two places:
//  1. main.go / initial model construction, with empty content.
//  2. model.go, when a file is opened from the tree (fresh Editor per open).
//
// Ctrl+Z/Y/X/C/V shortcuts are intercepted inside goeditorAdapter.interceptKey
// — there is no addCtrlBindings step here. The adapter's interception layer
// runs before goeditor sees the keystroke, so user-level bindings stay
// consistent across Normal/Insert/Visual without per-mode registration.
func newEditor(content, path string, opts ...*editorOpts) Editor {
	var o *editorOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	_, relNums := resolveEditorOpts(o)
	return newGoeditorAdapter(path, content, relNums)
}

// resolveEditorOpts returns the effective keymap + relative-numbers setting
// for a (possibly nil) editorOpts pointer. Split out so both newEditor and
// direct callers in tests can compute the same defaults without duplicating
// the condition logic.
func resolveEditorOpts(o *editorOpts) (config.Keymap, bool) {
	if o == nil {
		d := config.Defaults()
		return d.Keymap, d.UI.RelativeNumbers
	}
	km := o.Keymap
	if km == nil {
		km = config.DefaultKeymap()
	}
	return km, o.RelativeNumbers
}
