package plugin

// Activation-pattern semantics (for the test author in T9):
//
//   - "onStart" matches only the onStart event. It carries no value.
//   - "onLanguage:<lang>" matches an onLanguage event whose value equals
//     <lang> case-insensitively (e.g. "go" matches "Go").
//   - "onSave:<glob>" matches an onSave event whose value, compared by
//     filepath.Base, matches <glob> under path/filepath.Match. Basename-only
//     matching is intentional in v1: "onSave:*.go" works, but directory
//     patterns like "onSave:vendor/**" do not.
//   - "onCommand:<id>" matches an onCommand event whose value equals <id>
//     exactly.
//   - An unrecognized Kind is a parse error, never a silent false.
//   - A pattern other than "onStart" with an empty value is a parse error.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Activation Kind constants.
const (
	ActStart    = "onStart"
	ActLanguage = "onLanguage"
	ActSave     = "onSave"
	ActCommand  = "onCommand"
)

// ActivationEvent is a decoded activation trigger. Kind is one of the Act*
// constants; Value carries the kind-specific payload (empty for onStart).
type ActivationEvent struct {
	Kind  string
	Value string
}

// ParseActivation decodes a manifest activation string such as
// "onLanguage:go" into an ActivationEvent.
func ParseActivation(s string) (ActivationEvent, error) {
	kind, value, hasColon := strings.Cut(s, ":")
	switch kind {
	case ActStart:
		if hasColon {
			return ActivationEvent{}, fmt.Errorf("activation: %q: onStart takes no value", s)
		}
		return ActivationEvent{Kind: ActStart}, nil
	case ActLanguage, ActSave, ActCommand:
		if !hasColon || value == "" {
			return ActivationEvent{}, fmt.Errorf("activation: %q: %s requires a value", s, kind)
		}
		return ActivationEvent{Kind: kind, Value: value}, nil
	default:
		return ActivationEvent{}, fmt.Errorf("activation: %q: unknown kind %q", s, kind)
	}
}

// Match reports whether any pattern in patterns matches event. A malformed
// pattern is an error rather than a silent false.
func Match(patterns []string, event ActivationEvent) (bool, error) {
	for _, raw := range patterns {
		p, err := ParseActivation(raw)
		if err != nil {
			return false, err
		}
		ok, err := matchOne(p, event)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func matchOne(pattern, event ActivationEvent) (bool, error) {
	if pattern.Kind != event.Kind {
		return false, nil
	}
	switch pattern.Kind {
	case ActStart:
		return true, nil
	case ActLanguage:
		return strings.EqualFold(pattern.Value, event.Value), nil
	case ActCommand:
		return pattern.Value == event.Value, nil
	case ActSave:
		ok, err := filepath.Match(pattern.Value, filepath.Base(event.Value))
		if err != nil {
			return false, fmt.Errorf("activation: bad glob %q: %w", pattern.Value, err)
		}
		return ok, nil
	default:
		return false, fmt.Errorf("activation: unknown kind %q", pattern.Kind)
	}
}
