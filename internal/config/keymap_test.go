package config

import (
	"strings"
	"testing"
)

func TestDefaultKeymapAllActions(t *testing.T) {
	km := DefaultKeymap()
	for _, a := range knownActions {
		if km[a] == "" {
			t.Errorf("DefaultKeymap missing %q", a)
		}
	}
}

func TestKeymapValidateUnknownActionDropped(t *testing.T) {
	km := DefaultKeymap()
	km["does_not_exist"] = "ctrl+f"
	warnings := km.Validate()
	if len(warnings) == 0 {
		t.Fatal("expected warning for unknown action")
	}
	if _, still := km["does_not_exist"]; still {
		t.Error("unknown action should be dropped")
	}
	if !strings.Contains(warnings[0].Message, "unknown action") {
		t.Errorf("warning text: %q", warnings[0].Message)
	}
}

func TestKeymapValidateEmptyBindingFallsBack(t *testing.T) {
	km := DefaultKeymap()
	km[ActionSave] = ""
	warnings := km.Validate()
	if len(warnings) == 0 {
		t.Fatal("expected warning for empty binding")
	}
	if km[ActionSave] != "ctrl+s" {
		t.Errorf("empty save didn't fall back to default: %q", km[ActionSave])
	}
}

func TestKeymapValidateDuplicateBinding(t *testing.T) {
	km := DefaultKeymap()
	// Force a conflict between save and quit.
	km[ActionQuit] = "ctrl+s"
	warnings := km.Validate()
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Message, "duplicate binding") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected duplicate-binding warning, got %+v", warnings)
	}
	// Save was declared first among knownActions, so it wins the original
	// "ctrl+s". Quit falls back to the default "ctrl+q".
	if km[ActionSave] != "ctrl+s" {
		t.Errorf("save should have kept ctrl+s, got %q", km[ActionSave])
	}
	if km[ActionQuit] != "ctrl+q" {
		t.Errorf("quit should have fallen back to ctrl+q, got %q", km[ActionQuit])
	}
}

func TestKeymapValidateDuplicateFallbackAlsoTaken(t *testing.T) {
	// Construct a scenario where the fallback default is itself already
	// claimed. Per inline tie-break: keep the later binding in place.
	km := Keymap{
		ActionSave:    "ctrl+q", // collides with default for quit; save's default is ctrl+s
		ActionQuit:    "ctrl+s", // collides with default for save; quit's default is ctrl+q, which save now holds
		ActionPalette: "ctrl+p",
	}
	// After walk: save claims ctrl+q. quit's ctrl+s is not yet claimed, so
	// this specific case doesn't exercise "fallback also taken" — let's pick
	// a harsher construction.
	km = Keymap{
		ActionSave:    "ctrl+s",
		ActionQuit:    "ctrl+s", // duplicate; fallback=ctrl+q
		ActionPalette: "ctrl+q", // also taken (or will be)
	}
	warnings := km.Validate()
	if len(warnings) == 0 {
		t.Fatal("expected warnings")
	}
	// The point: no silent data loss — save still has ctrl+s.
	if km[ActionSave] != "ctrl+s" {
		t.Errorf("save lost binding: %q", km[ActionSave])
	}
}

func TestKeymapLookupFallsBackToDefault(t *testing.T) {
	km := Keymap{}
	if got := km.Lookup(ActionSave); got != "ctrl+s" {
		t.Errorf("Lookup(save) = %q, want ctrl+s (default)", got)
	}
}

func TestKeymapLookupUsesCustom(t *testing.T) {
	km := Keymap{ActionSave: "alt+s"}
	if got := km.Lookup(ActionSave); got != "alt+s" {
		t.Errorf("Lookup(save) = %q, want alt+s", got)
	}
}

func TestKeymapLookupNilReceiver(t *testing.T) {
	var km Keymap
	if got := km.Lookup(ActionQuit); got != "ctrl+q" {
		t.Errorf("nil Lookup(quit) = %q, want ctrl+q", got)
	}
}
