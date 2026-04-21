package plugin

// KeyEvent is a transport-neutral keyboard event delivered to panes.
//
// The host adapter converts bubbletea's tea.KeyMsg (or the equivalent wire
// message for out-of-process plugins) into a KeyEvent so plugins never need
// to depend on bubbletea.
//
// Key holds the canonical key name — e.g. "enter", "ctrl+c", "up", "esc",
// "tab", "backspace" — matching the vocabulary used by bubbletea's KeyMsg
// String form. Runes holds the printable rune sequence when applicable (for
// plain typed characters Key is empty and Runes contains the input). Alt is
// true when the Alt/Option modifier was held.
type KeyEvent struct {
	// Key is the canonical key name (e.g. "enter", "ctrl+c", "up"), or the
	// empty string for plain printable input.
	Key string `json:"key,omitempty"`
	// Runes holds printable runes for typed input, or nil for named keys.
	Runes []rune `json:"runes,omitempty"`
	// Alt reports whether the Alt/Option modifier was held.
	Alt bool `json:"alt,omitempty"`
}

// Pane is implemented by plugins that own a rectangular region of the
// editor's layout. Plugins that want a pane implement both Plugin and Pane.
type Pane interface {
	// Render returns the pane's content sized to the given width and height
	// in terminal cells. The host is responsible for drawing borders and
	// padding around the returned string.
	Render(w, h int) string

	// HandleKey is invoked for each key routed to the focused pane. If the
	// pane consumed the key, it must return at least one effect — or an
	// empty, non-nil slice — so the host knows the event was handled.
	// Returning nil signals the key was not recognized, allowing the host
	// to route it elsewhere.
	HandleKey(k KeyEvent) []Effect

	// OnResize is called whenever the pane's assigned region changes size.
	OnResize(w, h int)

	// OnFocus is called when the pane gains (focused == true) or loses
	// (focused == false) input focus.
	OnFocus(focused bool)

	// Slot returns the layout slot this pane occupies. The v1 vocabulary is
	// "left", "right", or "bottom"; the host model maps slots to screen
	// regions.
	Slot() string
}
