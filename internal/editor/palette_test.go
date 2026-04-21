package editor

import (
	"reflect"
	"testing"

	"github.com/savimcio/nistru/plugin"
)

// buildPalette returns a paletteModel with the given entries pre-loaded via
// Open. Entries are supplied as CommandRef map so the test exercises the same
// sorting and filtering code paths the production code runs.
func buildPalette(refs map[string]plugin.CommandRef) *paletteModel {
	p := &paletteModel{}
	p.Open(refs)
	return p
}

func TestEntriesFromCmds_SortOrderAndTitleFallback(t *testing.T) {
	refs := map[string]plugin.CommandRef{
		"z.open":    {Title: "alpha", Plugin: "one"},
		"a.explore": {Title: "alpha", Plugin: "two"}, // tiebreaker: ID < z.open
		"b.cmd":     {Title: "Beta", Plugin: "one"},
		"c.cmd":     {Title: "", Plugin: "three"}, // empty title falls back to ID
	}
	got := entriesFromCmds(refs)
	wantIDs := []string{"a.explore", "z.open", "b.cmd", "c.cmd"}
	if len(got) != len(wantIDs) {
		t.Fatalf("length mismatch: got %d, want %d (%v)", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("position %d: got ID=%q, want %q (full=%+v)", i, got[i].ID, id, got)
		}
	}
	// Title fallback: entry at index 3 has title == ID.
	if got[3].Title != "c.cmd" {
		t.Errorf("empty Title should fall back to ID, got %q", got[3].Title)
	}
}

func TestPaletteRefilter_Table(t *testing.T) {
	// Fixture: four commands with deliberate casing + overlap for substring
	// match assertions.
	refs := map[string]plugin.CommandRef{
		"save":     {Title: "Save", Plugin: "core"},
		"saveAll":  {Title: "Save All", Plugin: "core"},
		"open":     {Title: "Open File", Plugin: "core"},
		"greeter":  {Title: "Greet", Plugin: "hello"},
	}

	cases := []struct {
		name    string
		query   string
		wantIDs []string // expected filtered IDs in order
	}{
		{
			name:    "empty query returns all sorted",
			query:   "",
			wantIDs: []string{"greeter", "open", "save", "saveAll"},
		},
		{
			name:    "exact title match",
			query:   "Open File",
			wantIDs: []string{"open"},
		},
		{
			name:    "case-insensitive substring",
			query:   "SAVE",
			wantIDs: []string{"save", "saveAll"},
		},
		{
			name:    "matches plugin name",
			query:   "hello",
			wantIDs: []string{"greeter"},
		},
		{
			name:    "matches command ID",
			query:   "greeter",
			wantIDs: []string{"greeter"},
		},
		{
			name:    "no match returns empty",
			query:   "zzznonexistent",
			wantIDs: []string{},
		},
		{
			name:    "partial substring returns multiple, alpha order preserved",
			query:   "e",
			wantIDs: []string{"greeter", "open", "save", "saveAll"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := buildPalette(refs)
			p.query = tc.query
			p.refilter()
			gotIDs := make([]string, 0, len(p.filtered))
			for _, e := range p.filtered {
				gotIDs = append(gotIDs, e.ID)
			}
			if len(tc.wantIDs) == 0 && len(gotIDs) == 0 {
				return
			}
			if !reflect.DeepEqual(gotIDs, tc.wantIDs) {
				t.Errorf("query %q: got %v, want %v", tc.query, gotIDs, tc.wantIDs)
			}
		})
	}
}

func TestPaletteRefilter_CursorClampNoMatch(t *testing.T) {
	// When the filter produces zero results, refilter clamps the cursor to 0
	// (the -1 branch then flips it back to 0). HandleKey's Enter handler
	// guards len(filtered)>0 before dereferencing, so cursor==0 on an empty
	// filtered slice is safe.
	refs := map[string]plugin.CommandRef{
		"save": {Title: "Save", Plugin: "core"},
	}
	p := buildPalette(refs)
	p.cursor = 0
	p.query = "xxxxx"
	p.refilter()
	if len(p.filtered) != 0 {
		t.Fatalf("expected empty filtered, got %d", len(p.filtered))
	}
	if p.cursor != 0 {
		t.Errorf("cursor on empty filter should be clamped to 0, got %d", p.cursor)
	}
}

func TestPaletteRefilter_CursorClampDownFromEnd(t *testing.T) {
	refs := map[string]plugin.CommandRef{
		"save":    {Title: "Save"},
		"saveAll": {Title: "Save All"},
		"sum":     {Title: "Sum"},
	}
	p := buildPalette(refs)
	p.cursor = 2 // point at the last entry pre-filter
	p.query = "save"
	p.refilter()
	// Now only 2 entries match; cursor must be clamped to 1.
	if len(p.filtered) != 2 {
		t.Fatalf("expected 2 filtered, got %d", len(p.filtered))
	}
	if p.cursor != 1 {
		t.Errorf("cursor should be clamped to len-1=1, got %d", p.cursor)
	}
}

func TestPaletteHandleKey_CursorInvariantsAcrossRefilters(t *testing.T) {
	// Walk type 'a' → backspace → type 'b' and assert cursor bounds after each
	// mutation. The invariant: 0 <= cursor <= max(0, len(filtered)-1).
	refs := map[string]plugin.CommandRef{
		"save":    {Title: "Save"},
		"saveAll": {Title: "Save All"},
		"apple":   {Title: "Apple"},
		"banana":  {Title: "Banana"},
	}
	p := buildPalette(refs)

	check := func(label string) {
		t.Helper()
		if p.cursor < 0 {
			t.Fatalf("%s: cursor went negative: %d", label, p.cursor)
		}
		if len(p.filtered) > 0 && p.cursor > len(p.filtered)-1 {
			t.Fatalf("%s: cursor %d exceeds len-1=%d", label, p.cursor, len(p.filtered)-1)
		}
	}

	// Move cursor to the bottom entry first so refilter has meaningful work.
	for range 10 {
		p.HandleKey("down", nil)
		check("down-spam")
	}

	// Type 'a'
	if _, _ = p.HandleKey("", []rune{'a'}); p.query != "a" {
		t.Fatalf("expected query 'a', got %q", p.query)
	}
	check("after type a")

	// Backspace
	p.HandleKey("backspace", nil)
	if p.query != "" {
		t.Fatalf("expected empty query after backspace, got %q", p.query)
	}
	check("after backspace")

	// Type 'b'
	p.HandleKey("", []rune{'b'})
	if p.query != "b" {
		t.Fatalf("expected query 'b', got %q", p.query)
	}
	check("after type b")
}

func TestPaletteHandleKey_EnterReturnsSelectedEntry(t *testing.T) {
	refs := map[string]plugin.CommandRef{
		"save":    {Title: "Save"},
		"saveAll": {Title: "Save All"},
	}
	p := buildPalette(refs)
	p.cursor = 1

	closed, activated := p.HandleKey("enter", nil)
	if !closed {
		t.Errorf("enter on valid selection should close the palette")
	}
	if activated == nil {
		t.Fatalf("enter on valid selection should activate an entry")
	}
	if activated.ID != "saveAll" {
		t.Errorf("activated wrong entry: got %q, want %q", activated.ID, "saveAll")
	}
}

func TestPaletteHandleKey_EnterOnEmptyFilteredNoActivation(t *testing.T) {
	refs := map[string]plugin.CommandRef{
		"save": {Title: "Save"},
	}
	p := buildPalette(refs)
	p.query = "zzz"
	p.refilter()

	closed, activated := p.HandleKey("enter", nil)
	if closed {
		t.Errorf("enter on empty filtered should NOT close palette")
	}
	if activated != nil {
		t.Errorf("enter on empty filtered should not activate; got %+v", activated)
	}
}

func TestPaletteHandleKey_EscapeCloses(t *testing.T) {
	p := buildPalette(map[string]plugin.CommandRef{"a": {Title: "A"}})
	closed, activated := p.HandleKey("esc", nil)
	if !closed {
		t.Errorf("esc should close the palette")
	}
	if activated != nil {
		t.Errorf("esc should not activate anything")
	}
}

func TestPaletteHandleKey_IgnoresNonPrintableRunes(t *testing.T) {
	p := buildPalette(map[string]plugin.CommandRef{"a": {Title: "Alpha"}})
	// NUL + tab are non-printable — query should be unchanged.
	p.HandleKey("", []rune{0x00})
	if p.query != "" {
		t.Errorf("non-printable rune should not extend query; got %q", p.query)
	}
	p.HandleKey("", []rune{'\t'})
	if p.query != "" {
		t.Errorf("tab rune should not extend query; got %q", p.query)
	}
}

// TestPaletteLookupByID exercises the one-and-only lookup surface the palette
// exposes: entriesFromCmds returning the right entry for a given ID, and the
// palette's filtered list containing that entry after an exact-ID query.
func TestPaletteLookupByID(t *testing.T) {
	refs := map[string]plugin.CommandRef{
		"core.save": {Title: "Save", Plugin: "core"},
	}
	p := buildPalette(refs)
	p.query = "core.save"
	p.refilter()
	if len(p.filtered) != 1 {
		t.Fatalf("lookup by ID: expected 1 match, got %d", len(p.filtered))
	}
	if p.filtered[0].ID != "core.save" || p.filtered[0].Plugin != "core" {
		t.Errorf("lookup by ID: wrong entry: %+v", p.filtered[0])
	}

	// Miss: an ID that does not exist returns zero entries.
	p.query = "does.not.exist"
	p.refilter()
	if len(p.filtered) != 0 {
		t.Errorf("lookup miss: expected 0, got %d", len(p.filtered))
	}
}

func TestPaletteOpenResetsStateAcrossCalls(t *testing.T) {
	refs1 := map[string]plugin.CommandRef{"a": {Title: "A"}, "b": {Title: "B"}}
	refs2 := map[string]plugin.CommandRef{"x": {Title: "X"}}

	p := &paletteModel{}
	p.Open(refs1)
	p.query = "ignored"
	p.cursor = 1

	p.Open(refs2)
	if p.query != "" {
		t.Errorf("Open should reset query, got %q", p.query)
	}
	if p.cursor != 0 {
		t.Errorf("Open should reset cursor, got %d", p.cursor)
	}
	if len(p.all) != 1 || p.all[0].ID != "x" {
		t.Errorf("Open should rebuild 'all' from new refs, got %+v", p.all)
	}
	if !p.open {
		t.Errorf("Open should mark palette open")
	}
	p.Close()
	if p.open {
		t.Errorf("Close should clear open flag")
	}
}
