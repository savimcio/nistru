package plugin

import (
	"strings"
	"testing"
)

func TestParseActivation_Roundtrip(t *testing.T) {
	cases := []struct {
		in     string
		kind   string
		value  string
		errStr string // non-empty if an error is expected
	}{
		{in: "onStart", kind: ActStart, value: ""},
		{in: "onLanguage:go", kind: ActLanguage, value: "go"},
		{in: "onSave:*.go", kind: ActSave, value: "*.go"},
		{in: "onCommand:fmt", kind: ActCommand, value: "fmt"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			ev, err := ParseActivation(c.in)
			if err != nil {
				t.Fatalf("ParseActivation(%q): unexpected error: %v", c.in, err)
			}
			if ev.Kind != c.kind {
				t.Fatalf("Kind = %q, want %q", ev.Kind, c.kind)
			}
			if ev.Value != c.value {
				t.Fatalf("Value = %q, want %q", ev.Value, c.value)
			}
		})
	}
}

func TestParseActivation_EmptyValueErrors(t *testing.T) {
	cases := []string{
		"onLanguage:",
		"onLanguage",
		"onSave:",
		"onSave",
		"onCommand:",
		"onCommand",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := ParseActivation(s)
			if err == nil {
				t.Fatalf("ParseActivation(%q): expected error, got nil", s)
			}
		})
	}
}

func TestParseActivation_UnknownKind(t *testing.T) {
	_, err := ParseActivation("onHover:foo")
	if err == nil {
		t.Fatalf("ParseActivation: expected error for unknown kind")
	}
}

func TestParseActivation_OnStartWithValueErrors(t *testing.T) {
	_, err := ParseActivation("onStart:something")
	if err == nil {
		t.Fatalf("ParseActivation: expected error for onStart with value")
	}
}

func TestMatch_Matrix(t *testing.T) {
	cases := []struct {
		desc     string
		patterns []string
		event    ActivationEvent
		want     bool
		wantErr  bool
	}{
		{
			desc:     "onStart vs onStart -> true",
			patterns: []string{"onStart"},
			event:    ActivationEvent{Kind: ActStart},
			want:     true,
		},
		{
			desc:     "onStart vs onLanguage/go -> false",
			patterns: []string{"onStart"},
			event:    ActivationEvent{Kind: ActLanguage, Value: "go"},
			want:     false,
		},
		{
			desc:     "onLanguage:Go (capital) vs onLanguage/go -> true (case-insensitive)",
			patterns: []string{"onLanguage:Go"},
			event:    ActivationEvent{Kind: ActLanguage, Value: "go"},
			want:     true,
		},
		{
			desc:     "onSave:*.go vs onSave/path/to/foo.go -> true (basename)",
			patterns: []string{"onSave:*.go"},
			event:    ActivationEvent{Kind: ActSave, Value: "/path/to/foo.go"},
			want:     true,
		},
		{
			desc:     "onSave:*.go vs onSave/path/to/foo.py -> false",
			patterns: []string{"onSave:*.go"},
			event:    ActivationEvent{Kind: ActSave, Value: "/path/to/foo.py"},
			want:     false,
		},
		{
			desc:     "onSave:foo*.go vs onSave/foo_test.go -> true",
			patterns: []string{"onSave:foo*.go"},
			event:    ActivationEvent{Kind: ActSave, Value: "foo_test.go"},
			want:     true,
		},
		{
			desc:     "onCommand:fmt vs onCommand/fmt -> true",
			patterns: []string{"onCommand:fmt"},
			event:    ActivationEvent{Kind: ActCommand, Value: "fmt"},
			want:     true,
		},
		{
			desc:     "onCommand:fmt vs onCommand/xfmt -> false",
			patterns: []string{"onCommand:fmt"},
			event:    ActivationEvent{Kind: ActCommand, Value: "xfmt"},
			want:     false,
		},
		{
			desc:     "unknown pattern kind -> error",
			patterns: []string{"onHover:x"},
			event:    ActivationEvent{Kind: ActLanguage, Value: "go"},
			wantErr:  true,
		},
		{
			desc:     "mixed patterns, one matches -> true",
			patterns: []string{"onStart", "onLanguage:go"},
			event:    ActivationEvent{Kind: ActLanguage, Value: "go"},
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := Match(tc.patterns, tc.event)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Match: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Match: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatch_MalformedGlob(t *testing.T) {
	// A glob with an unmatched `[` is malformed under path/filepath.Match.
	_, err := Match([]string{"onSave:[abc"}, ActivationEvent{Kind: ActSave, Value: "foo.go"})
	if err == nil {
		t.Fatalf("Match: expected glob error, got nil")
	}
	if !strings.Contains(err.Error(), "activation") {
		t.Fatalf("Match: error %q missing 'activation'", err.Error())
	}
}
