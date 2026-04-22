package config

import (
	"fmt"
	"sort"
)

// Action enumerates the editor commands that can be bound to a key.
type Action string

const (
	ActionSave      Action = "save"
	ActionQuit      Action = "quit"
	ActionPalette   Action = "palette"
	ActionFocusNext Action = "focus_next"
	ActionFocusPrev Action = "focus_prev"
	ActionUndo      Action = "undo"
	ActionRedo      Action = "redo"
	ActionCutLine   Action = "cut_line"
	ActionCopyLine  Action = "copy_line"
	ActionPaste     Action = "paste"
)

// Keymap maps actions to their bound key string (bubbletea KeyMsg.String()
// form, e.g. "ctrl+s", "shift+tab"). It is NOT validated against the full
// bubbletea key vocabulary — that list is open-ended and using a whitelist
// would produce spurious warnings. Only structural problems (unknown
// actions, empty strings, duplicates) are flagged.
type Keymap map[Action]string

// knownActions is the canonical list of actions in their stable source
// order — used for duplicate detection (earlier declaration wins).
var knownActions = []Action{
	ActionSave, ActionQuit, ActionPalette,
	ActionFocusNext, ActionFocusPrev,
	ActionUndo, ActionRedo,
	ActionCutLine, ActionCopyLine, ActionPaste,
}

// DefaultKeymap returns the built-in default bindings.
func DefaultKeymap() Keymap {
	return Keymap{
		ActionSave:      "ctrl+s",
		ActionQuit:      "ctrl+q",
		ActionPalette:   "ctrl+p",
		ActionFocusNext: "tab",
		ActionFocusPrev: "shift+tab",
		ActionUndo:      "ctrl+z",
		ActionRedo:      "ctrl+y",
		ActionCutLine:   "ctrl+x",
		ActionCopyLine:  "ctrl+c",
		ActionPaste:     "ctrl+v",
	}
}

// isKnownAction reports whether a is a declared Action.
func isKnownAction(a Action) bool {
	for _, k := range knownActions {
		if k == a {
			return true
		}
	}
	return false
}

// Validate mutates the receiver in place to resolve structural problems
// and returns a slice of human-readable warnings:
//
//   - Unknown actions (keys that don't match any Action constant) are
//     dropped.
//   - Empty bindings fall back to the default for that action.
//   - Duplicate bindings (two actions sharing the same key) fall back on
//     the later-declared action. If the fallback ALSO conflicts with an
//     earlier mapping, the later binding is kept as-is — we'd rather
//     leave a conflict in place than silently lose a binding.
func (k Keymap) Validate() []Warning {
	if k == nil {
		return nil
	}
	var warnings []Warning

	// Drop unknown actions first — deterministic order for test stability.
	unknown := make([]string, 0)
	for a := range k {
		if !isKnownAction(a) {
			unknown = append(unknown, string(a))
		}
	}
	sort.Strings(unknown)
	for _, u := range unknown {
		warnings = append(warnings, Warning{
			Source:  "keymap",
			Message: fmt.Sprintf("unknown action %q (dropped)", u),
		})
		delete(k, Action(u))
	}

	defaults := DefaultKeymap()

	// Empty bindings → fall back to default.
	for _, a := range knownActions {
		if v, ok := k[a]; ok && v == "" {
			warnings = append(warnings, Warning{
				Source:  "keymap",
				Message: fmt.Sprintf("empty binding for %q (using default %q)", a, defaults[a]),
			})
			k[a] = defaults[a]
		}
	}

	// Duplicate detection walks actions in declaration order. The first to
	// claim a key keeps it; later ones are bumped back to their default.
	claimed := map[string]Action{}
	for _, a := range knownActions {
		v, ok := k[a]
		if !ok {
			continue
		}
		if prev, taken := claimed[v]; taken {
			// Tie-break: try the default for the losing action. If that
			// default is also taken, keep the current value (document the
			// conflict; don't lose the binding).
			fallback := defaults[a]
			if _, taken2 := claimed[fallback]; taken2 || fallback == v {
				warnings = append(warnings, Warning{
					Source:  "keymap",
					Message: fmt.Sprintf("duplicate binding %q (actions %q and %q share it; keeping %q)", v, prev, a, a),
				})
				claimed[v] = a
				continue
			}
			warnings = append(warnings, Warning{
				Source:  "keymap",
				Message: fmt.Sprintf("duplicate binding %q (actions %q and %q share it; %q falls back to %q)", v, prev, a, a, fallback),
			})
			k[a] = fallback
			claimed[fallback] = a
			continue
		}
		claimed[v] = a
	}
	return warnings
}

// Lookup returns the binding for a, or the built-in default when the
// receiver has no entry (e.g. after Validate dropped a broken one).
func (k Keymap) Lookup(a Action) string {
	if k != nil {
		if v, ok := k[a]; ok && v != "" {
			return v
		}
	}
	return DefaultKeymap()[a]
}
