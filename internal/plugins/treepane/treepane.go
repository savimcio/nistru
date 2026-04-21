// Package treepane is the built-in file-tree pane plugin for nistru.
//
// It implements both plugin.Plugin and plugin.Pane, exposing the existing
// tree navigation UI through the transport-neutral plugin API. The behavior
// is a direct port of the previous root-level tree.go; no visual or
// navigational changes are introduced here.
package treepane

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/savimcio/nistru/plugin"
)

// skipDirs is the hardcoded list of directory names we refuse to descend into.
// Anything hidden (prefix ".") is also skipped except the root itself.
var skipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
}

// row is one fully pre-rendered line of the tree. `prefix` contains the
// complete tree-art indent (ancestor bars + connector), so View is just
// prefix+label per row.
type row struct {
	path   string // absolute fs path
	label  string // basename; directories keep a trailing "/"
	prefix string // fully precomputed tree-art prefix, e.g. "│  ├── " or "   └── "
	isDir  bool
}

// fsNode is the in-memory tree representation. The full filesystem subtree is
// built eagerly at startup, then each directory's `expanded` flag controls
// whether its children contribute to the rendered row list at refresh time.
type fsNode struct {
	path     string // absolute fs path
	label    string // basename; directories DO NOT include trailing slash — added at render time
	isDir    bool
	children []*fsNode // directories only
	expanded bool      // directories only; false = collapsed
}

// buildFsTree walks the directory at root and returns the fully constructed
// fsNode tree. Every real directory starts collapsed; the synthetic top-level
// wrapper is expanded so its children are visible at launch.
func buildFsTree(root string) (*fsNode, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	children, err := walkDir(absRoot, true)
	if err != nil {
		return nil, err
	}

	rootNode := &fsNode{
		path:     absRoot,
		label:    filepath.Base(absRoot),
		isDir:    true,
		children: children,
		expanded: true,
	}
	return rootNode, nil
}

// walkDir reads dir and returns its children as *fsNode values. isRoot tells
// us whether to honour the "skip hidden" rule (we always descend into the
// root even if its own name starts with "."). Errors for individual entries
// are swallowed silently — a read-protected sub-tree shouldn't bring the UI
// down — but errors on the top-level read are propagated.
func walkDir(dir string, isRoot bool) ([]*fsNode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if isRoot {
			return nil, err
		}
		return nil, nil
	}

	// Split into dirs and files, filtering skip list.
	var dirs, files []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if _, skip := skipDirs[name]; skip {
			continue
		}
		if strings.HasPrefix(name, ".") {
			// Hidden entries are skipped below the root. We still show the
			// root itself regardless of its name (handled in buildFsTree).
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e)
		} else if e.Type().IsRegular() {
			files = append(files, e)
		}
	}

	byName := func(s []os.DirEntry) {
		sort.Slice(s, func(i, j int) bool {
			return strings.ToLower(s[i].Name()) < strings.ToLower(s[j].Name())
		})
	}
	byName(dirs)
	byName(files)

	nodes := make([]*fsNode, 0, len(dirs)+len(files))
	for _, d := range dirs {
		sub, _ := walkDir(filepath.Join(dir, d.Name()), false)
		nodes = append(nodes, &fsNode{
			path:     filepath.Join(dir, d.Name()),
			label:    d.Name(),
			isDir:    true,
			children: sub,
			expanded: false,
		})
	}
	for _, f := range files {
		nodes = append(nodes, &fsNode{
			path:  filepath.Join(dir, f.Name()),
			label: f.Name(),
			isDir: false,
		})
	}
	return nodes, nil
}

// Styling for the tree component.
var (
	treeSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	treeDirStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
)

// treeModel is a minimal, self-contained tree view. It owns its own
// navigation keys (j/k, arrows, g/G, ctrl+d/u, pgup/pgdn) plus
// collapse/expand keys (enter, h/l, left/right) so the parent Model does not
// need to special-case them.
type treeModel struct {
	root   *fsNode
	rows   []row // derived — rebuilt whenever expansion state changes
	cursor int
	offset int // top-visible-row index for scrolling
	width  int
	height int
}

// newTreeModel builds a tree model rooted at root and seeds the viewport.
// The initial row list is derived via refresh().
func newTreeModel(root *fsNode, w, h int) treeModel {
	if h < 1 {
		h = 1
	}
	if w < 1 {
		w = 1
	}
	m := treeModel{
		root:   root,
		width:  w,
		height: h,
	}
	m.refresh()
	return m
}

// refresh rebuilds m.rows by walking the fsNode tree, emitting only children
// whose parent is expanded. Prefix art is recomputed from the currently
// visible sibling order so collapsed subtrees don't influence their siblings'
// `├──` vs `└──` connectors.
func (m *treeModel) refresh() {
	rows := make([]row, 0, 64)
	if m.root != nil {
		emitNode(m.root, "", true, true, &rows)
	}
	m.rows = rows
}

// emitNode appends the row for node, then recursively emits its children if
// the node is an expanded directory. ancestorPrefix is the accumulated
// "│  "/"   " string from all ancestors (empty for the synthetic root).
// isLast controls this node's connector; isRoot suppresses it entirely so the
// top-level line is bare.
func emitNode(node *fsNode, ancestorPrefix string, isLast, isRoot bool, out *[]row) {
	var prefix string
	if isRoot {
		prefix = ""
	} else {
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		prefix = ancestorPrefix + connector
	}

	label := node.label
	if node.isDir {
		marker := "▸ "
		if node.expanded {
			marker = "▾ "
		}
		label = marker + label + "/"
	}

	*out = append(*out, row{
		path:   node.path,
		label:  label,
		prefix: prefix,
		isDir:  node.isDir,
	})

	if !node.isDir || !node.expanded {
		return
	}

	// Children inherit the ancestor prefix plus a segment for *this* node:
	// a vertical bar if this node has a following sibling, three spaces if
	// not. The synthetic root contributes nothing (it has no siblings).
	var childAncestor string
	if isRoot {
		childAncestor = ""
	} else if isLast {
		childAncestor = ancestorPrefix + "   "
	} else {
		childAncestor = ancestorPrefix + "│  "
	}

	for i, c := range node.children {
		emitNode(c, childAncestor, i == len(node.children)-1, false, out)
	}
}

// Cursor returns the currently selected row index.
func (m treeModel) Cursor() int { return m.cursor }

// SetCursor jumps the cursor to c (clamped) and adjusts scroll offset.
func (m *treeModel) SetCursor(c int) {
	m.cursor = clamp(c, 0, m.lastIndex())
	m.adjustOffset()
}

// SetSize resizes the viewport and re-clamps the scroll offset.
func (m *treeModel) SetSize(w, h int) {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	m.width = w
	m.height = h
	m.adjustOffset()
}

// Update handles navigation, collapse/expand, and file-activation keys. All
// other keys are ignored. The returned effect slice is non-nil only when the
// Enter key opens a file; directory toggles and cursor moves return nil.
func (m treeModel) Update(key string) (treeModel, []plugin.Effect) {
	switch key {
	case "j", "down":
		m.cursor++
	case "k", "up":
		m.cursor--
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = m.lastIndex()
	case "ctrl+d", "pgdown":
		m.cursor += m.height / 2
	case "ctrl+u", "pgup":
		m.cursor -= m.height / 2

	case "enter":
		if node := m.nodeAtCursor(); node != nil {
			if node.isDir {
				node.expanded = !node.expanded
				m.refreshPreservingCursor(node.path)
				return m, nil
			}
			p := node.path
			return m, []plugin.Effect{plugin.OpenFile{Path: p}}
		}
		return m, nil

	case "l", "right":
		if node := m.nodeAtCursor(); node != nil && node.isDir && !node.expanded {
			node.expanded = true
			m.refreshPreservingCursor(node.path)
		}
		return m, nil

	case "h", "left":
		if node := m.nodeAtCursor(); node != nil {
			if node.isDir && node.expanded {
				node.expanded = false
				m.refreshPreservingCursor(node.path)
				return m, nil
			}
			// Collapsed dir or file: jump cursor to parent's row, if any.
			if parent := m.findParent(node); parent != nil && parent != m.root {
				if idx := m.indexOfPath(parent.path); idx >= 0 {
					m.cursor = idx
					m.adjustOffset()
				}
			}
		}
		return m, nil

	default:
		return m, nil
	}
	m.cursor = clamp(m.cursor, 0, m.lastIndex())
	m.adjustOffset()
	return m, nil
}

// nodeAtCursor returns the fsNode corresponding to the row currently under
// the cursor, or nil if there is none.
func (m treeModel) nodeAtCursor() *fsNode {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return m.findNodeByPath(m.root, m.rows[m.cursor].path)
}

// findNodeByPath does a DFS search for a node matching path. The tree is
// small enough (one file's worth of directory) that a linear walk is fine.
func (m treeModel) findNodeByPath(node *fsNode, path string) *fsNode {
	if node == nil {
		return nil
	}
	if node.path == path {
		return node
	}
	for _, c := range node.children {
		if found := m.findNodeByPath(c, path); found != nil {
			return found
		}
	}
	return nil
}

// findParent returns the parent of target, or nil if target is the root or
// absent from the tree.
func (m treeModel) findParent(target *fsNode) *fsNode {
	if target == nil || m.root == nil || target == m.root {
		return nil
	}
	return findParentHelper(m.root, target)
}

func findParentHelper(cur, target *fsNode) *fsNode {
	for _, c := range cur.children {
		if c == target {
			return cur
		}
		if found := findParentHelper(c, target); found != nil {
			return found
		}
	}
	return nil
}

// indexOfPath returns the index of the row whose path matches, or -1.
func (m treeModel) indexOfPath(path string) int {
	for i, r := range m.rows {
		if r.path == path {
			return i
		}
	}
	return -1
}

// refreshPreservingCursor rebuilds rows, then tries to keep the cursor on the
// same logical node. `preferPath` is a hint (usually the toggled node's path)
// that we try first; otherwise we fall back to the pre-refresh cursor path
// and then to the nearest visible ancestor.
func (m *treeModel) refreshPreservingCursor(preferPath string) {
	prevPath := ""
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		prevPath = m.rows[m.cursor].path
	}

	m.refresh()

	// 1. Prefer the explicit hint (the toggled directory stays under cursor).
	if preferPath != "" {
		if idx := m.indexOfPath(preferPath); idx >= 0 {
			m.cursor = idx
			m.clampAndAdjust()
			return
		}
	}
	// 2. Same node as before the refresh, if still visible.
	if prevPath != "" {
		if idx := m.indexOfPath(prevPath); idx >= 0 {
			m.cursor = idx
			m.clampAndAdjust()
			return
		}
		// 3. Walk up ancestors until we find one that's still visible.
		if node := m.findNodeByPath(m.root, prevPath); node != nil {
			for p := m.findParent(node); p != nil; p = m.findParent(p) {
				if idx := m.indexOfPath(p.path); idx >= 0 {
					m.cursor = idx
					m.clampAndAdjust()
					return
				}
			}
		}
	}
	m.clampAndAdjust()
}

// clampAndAdjust bounds the cursor/offset after a refresh.
func (m *treeModel) clampAndAdjust() {
	m.cursor = clamp(m.cursor, 0, m.lastIndex())
	m.adjustOffset()
}

// View renders the visible slice of rows. Selected row has its *label*
// highlighted; the prefix stays default so the tree art remains legible.
func (m treeModel) View() string {
	if len(m.rows) == 0 || m.height < 1 {
		return ""
	}
	end := m.offset + m.height
	end = min(end, len(m.rows))
	lines := make([]string, 0, end-m.offset)
	for i := m.offset; i < end; i++ {
		r := m.rows[i]
		var label string
		switch {
		case i == m.cursor:
			label = treeSelectedStyle.Render(r.label)
		case r.isDir:
			label = treeDirStyle.Render(r.label)
		default:
			label = r.label
		}
		line := r.prefix + label
		lines = append(lines, truncateToWidth(line, m.width))
	}
	return strings.Join(lines, "\n")
}

// lastIndex returns the index of the final row, or 0 for an empty list.
func (m treeModel) lastIndex() int {
	if len(m.rows) == 0 {
		return 0
	}
	return len(m.rows) - 1
}

// adjustOffset ensures cursor is inside [offset, offset+height) and keeps
// offset within legal bounds.
func (m *treeModel) adjustOffset() {
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
	maxOffset := max(len(m.rows)-m.height, 0)
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// truncateToWidth trims s to at most width visible cells, measured with
// lipgloss.Width so ANSI escapes don't count. We operate on runes, not
// bytes, so we never split a multibyte character.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	// Binary-search-free simple cut: drop runes from the end until width fits.
	// Works fine for the short tree lines we render.
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// TreePane is the exported wrapper that implements both plugin.Plugin and
// plugin.Pane. It owns a treeModel and forwards pane lifecycle calls into it
// while translating transport-neutral KeyEvents into the string keys that
// treeModel.Update expects.
type TreePane struct {
	model   treeModel
	root    string
	w, h    int
	focused bool
}

// New constructs a TreePane rooted at rootPath. It builds the full filesystem
// tree eagerly, matching the previous behavior. Returns an error if the root
// directory cannot be read.
func New(rootPath string) (*TreePane, error) {
	node, err := buildFsTree(rootPath)
	if err != nil {
		return nil, err
	}
	return &TreePane{
		model: newTreeModel(node, 1, 1),
		root:  rootPath,
	}, nil
}

// Name returns the plugin's stable identifier.
func (p *TreePane) Name() string { return "treepane" }

// Activation returns the activation event patterns. The host does not use
// this for in-process pane plugins in v1, but it is returned for consistency
// with the wire protocol.
func (p *TreePane) Activation() []string { return []string{"onStart"} }

// OnEvent is a no-op in v1; the tree does not react to buffer events.
func (p *TreePane) OnEvent(event any) []plugin.Effect { return nil }

// Shutdown releases resources. The tree has none.
func (p *TreePane) Shutdown() error { return nil }

// Render returns the pane's content sized to w x h. If the requested size
// differs from the cached size, the underlying treeModel is resized before
// rendering.
func (p *TreePane) Render(w, h int) string {
	if w != p.w || h != p.h {
		p.w = w
		p.h = h
		p.model.SetSize(w, h)
	}
	return p.model.View()
}

// HandleKey translates a transport-neutral KeyEvent into the string key
// vocabulary understood by treeModel.Update and forwards it. Unknown keys
// return nil so the host may route them elsewhere.
func (p *TreePane) HandleKey(k plugin.KeyEvent) []plugin.Effect {
	var key string
	switch k.Key {
	case "j", "k", "down", "up", "g", "G",
		"ctrl+d", "pgdown", "ctrl+u", "pgup",
		"enter", "l", "right", "h", "left":
		key = k.Key
	default:
		return nil
	}
	newModel, effects := p.model.Update(key)
	p.model = newModel
	return effects
}

// OnResize caches the new size and applies it to the underlying model.
func (p *TreePane) OnResize(w, h int) {
	p.w = w
	p.h = h
	p.model.SetSize(w, h)
}

// OnFocus caches the new focus state. The tree does not currently vary its
// rendering based on focus, so nothing else happens.
func (p *TreePane) OnFocus(focused bool) {
	p.focused = focused
}

// Slot returns the layout slot this pane occupies.
func (p *TreePane) Slot() string { return "left" }
