package main

import (
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"

	"github.com/savimcio/nistru/plugin"
)

// paletteModel is the command-palette overlay. It is a small standalone
// component owned by the top-level Model and toggled with Ctrl+P. When open,
// it intercepts every key event until the user dismisses it (Esc) or picks a
// command (Enter).
type paletteModel struct {
	open     bool
	query    string
	filtered []paletteEntry
	cursor   int

	// all is the full, alpha-sorted snapshot captured at Open time. Kept
	// separate from filtered so backspace can widen the filter back out.
	all []paletteEntry
}

// paletteEntry is a flattened view of a plugin.CommandRef with its own ID
// attached, suitable for sorting and filtering independently of the host's
// command map layout.
type paletteEntry struct {
	ID     string
	Title  string
	Plugin string
}

// Open (re)opens the palette, rebuilds the sorted entry list from cmds
// (alpha order by Title, then ID as tiebreaker), clears the query, and
// moves the cursor to the top.
func (m *paletteModel) Open(cmds map[string]plugin.CommandRef) {
	m.open = true
	m.query = ""
	m.cursor = 0
	m.all = entriesFromCmds(cmds)
	m.filtered = append(m.filtered[:0], m.all...)
}

// Close dismisses the palette. Keeps the entry lists in place — the next
// Open will rebuild them from a fresh command snapshot.
func (m *paletteModel) Close() {
	m.open = false
}

// HandleKey processes a single key event while the palette is open.
// Returns closed=true if the overlay should now close (Esc, or Enter with a
// valid selection). Returns activated non-nil if the user pressed Enter on a
// filtered entry; the caller should invoke that command.
func (m *paletteModel) HandleKey(key string, runes []rune) (closed bool, activated *paletteEntry) {
	switch key {
	case "esc":
		return true, nil
	case "enter":
		if len(m.filtered) > 0 && m.cursor >= 0 && m.cursor < len(m.filtered) {
			entry := m.filtered[m.cursor]
			return true, &entry
		}
		return false, nil
	case "up", "ctrl+k":
		if m.cursor > 0 {
			m.cursor--
		}
		return false, nil
	case "down", "ctrl+j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
		return false, nil
	case "backspace":
		if len(m.query) > 0 {
			q := []rune(m.query)
			m.query = string(q[:len(q)-1])
			m.refilter()
		}
		return false, nil
	}

	// Printable runes extend the query. We require every rune to be
	// printable so we don't accidentally absorb composite key names (e.g.
	// "tab") — bubbletea only populates Runes for KeyRunes, but guard
	// anyway.
	if len(runes) == 0 {
		return false, nil
	}
	for _, r := range runes {
		if !unicode.IsPrint(r) {
			return false, nil
		}
	}
	m.query += string(runes)
	m.refilter()
	return false, nil
}

// refilter recomputes m.filtered from m.all using the current query and
// clamps the cursor into the new range. Alpha order is preserved because
// m.all is already sorted and we walk it in order.
func (m *paletteModel) refilter() {
	q := strings.ToLower(m.query)
	if q == "" {
		m.filtered = append(m.filtered[:0], m.all...)
	} else {
		out := m.filtered[:0]
		if cap(out) < len(m.all) {
			out = make([]paletteEntry, 0, len(m.all))
		}
		for _, e := range m.all {
			hay := strings.ToLower(e.Title + " " + e.ID + " " + e.Plugin)
			if strings.Contains(hay, q) {
				out = append(out, e)
			}
		}
		m.filtered = out
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// entriesFromCmds flattens a CommandRef map into a sorted entry slice.
// Sort order: case-insensitive Title, then ID as tiebreaker.
func entriesFromCmds(cmds map[string]plugin.CommandRef) []paletteEntry {
	out := make([]paletteEntry, 0, len(cmds))
	for id, ref := range cmds {
		title := ref.Title
		if title == "" {
			title = id
		}
		out = append(out, paletteEntry{
			ID:     id,
			Title:  title,
			Plugin: ref.Plugin,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Title), strings.ToLower(out[j].Title)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// View renders the palette as a bordered box centered in (width, height).
// The allCmds parameter is accepted for interface symmetry but not used —
// the palette reads from its cached filtered list (populated at Open).
func (m *paletteModel) View(width, height int, _ map[string]plugin.CommandRef) string {
	// Box width: ~60% of screen, clamped to [40, 100], and not wider than
	// the terminal minus a 1-col margin on each side.
	boxW := max(width*6/10, 40)
	boxW = min(boxW, 100)
	boxW = min(boxW, width-2)
	boxW = max(boxW, 10)

	// Leave room for border (2 lines) + input line + two separators +
	// status line = 6 lines of chrome.
	innerH := max(height-6, 1)

	inputStyle := lipgloss.NewStyle().
		Width(boxW - 4).
		Foreground(lipgloss.Color("252"))
	input := inputStyle.Render("> " + m.query)

	sepStyle := lipgloss.NewStyle().
		Width(boxW - 4).
		Foreground(lipgloss.Color("240"))
	sep := sepStyle.Render(strings.Repeat("─", boxW-4))

	rowStyle := lipgloss.NewStyle().Width(boxW - 4)
	selStyle := lipgloss.NewStyle().
		Width(boxW - 4).
		Foreground(lipgloss.Color("205")).
		Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	// Scroll window: keep the cursor visible within innerH rows.
	start := 0
	if m.cursor >= innerH {
		start = m.cursor - innerH + 1
	}
	end := min(start+innerH, len(m.filtered))

	var rows []string
	if len(m.filtered) == 0 {
		rows = append(rows, rowStyle.Render(dimStyle.Render("  (no commands)")))
	} else {
		for i := start; i < end; i++ {
			e := m.filtered[i]
			label := "  " + e.Title
			if e.Plugin != "" {
				label += "  " + dimStyle.Render("["+e.Plugin+"]")
			}
			if i == m.cursor {
				rows = append(rows, selStyle.Render(label))
			} else {
				rows = append(rows, rowStyle.Render(label))
			}
		}
	}
	// Pad rows up to innerH so the box has a stable height.
	for len(rows) < innerH {
		rows = append(rows, rowStyle.Render(""))
	}

	statusStyle := lipgloss.NewStyle().
		Width(boxW - 4).
		Foreground(lipgloss.Color("244"))
	status := statusStyle.Render(paletteStatus(m))

	body := lipgloss.JoinVertical(lipgloss.Left,
		input,
		sep,
		lipgloss.JoinVertical(lipgloss.Left, rows...),
		sep,
		status,
	)

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(0, 1).
		Render(body)

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// paletteStatus returns the footer hint shown at the bottom of the box.
func paletteStatus(m *paletteModel) string {
	if len(m.filtered) == 0 {
		return "esc close"
	}
	return "up/down navigate - enter run - esc close"
}
