package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

// Registry owns the set of in-process Plugin implementations and the set of
// discovered out-of-process manifests. It is not safe for concurrent use; the
// host builds the registry at startup on a single goroutine.
type Registry struct {
	inProc    []Plugin
	manifests []*Manifest
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// RegisterInProc adds an in-process plugin. Panics if a plugin with the same
// Name is already registered.
func (r *Registry) RegisterInProc(p Plugin) {
	name := p.Name()
	for _, existing := range r.inProc {
		if existing.Name() == name {
			panic(fmt.Sprintf("plugin: in-proc plugin %q already registered", name))
		}
	}
	r.inProc = append(r.inProc, p)
}

// Discover scans the user config dir and the project .nistru/plugins dir for
// plugin.json manifests. Project manifests override user manifests on name
// collision. Missing roots are silently ignored; unreadable roots are an
// error. A single broken manifest within a root is logged to stderr and
// skipped so one bad plugin does not block startup.
func (r *Registry) Discover(rootPath string) error {
	userRoot, err := userPluginsRoot()
	if err != nil {
		return err
	}
	projectRoot := filepath.Join(rootPath, ".nistru", "plugins")

	userManifests, err := loadManifestsFromRoot(userRoot)
	if err != nil {
		return err
	}
	projectManifests, err := loadManifestsFromRoot(projectRoot)
	if err != nil {
		return err
	}

	byName := make(map[string]*Manifest, len(userManifests)+len(projectManifests))
	order := make([]string, 0, len(userManifests)+len(projectManifests))
	add := func(m *Manifest) {
		if _, exists := byName[m.Name]; !exists {
			order = append(order, m.Name)
		}
		byName[m.Name] = m
	}
	for _, m := range userManifests {
		add(m)
	}
	for _, m := range projectManifests {
		if _, clash := byName[m.Name]; clash {
			fmt.Fprintf(os.Stderr, "plugin: project manifest %q overrides user manifest\n", m.Name)
		}
		add(m)
	}

	r.manifests = r.manifests[:0]
	for _, name := range order {
		r.manifests = append(r.manifests, byName[name])
	}
	return nil
}

// userPluginsRoot returns the plugins dir under the user's config dir, or an
// empty string if the OS does not expose one (treated as "no user root").
func userPluginsRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		// No user config dir available — treat as "no user root" rather than
		// a hard failure so discovery still works in minimal environments.
		return "", nil
	}
	return filepath.Join(dir, "nistru", "plugins"), nil
}

// loadManifestsFromRoot reads every <root>/<plugin>/plugin.json. A missing
// root returns (nil, nil). An unreadable root returns an error. Individual
// manifest failures are logged and skipped.
func loadManifestsFromRoot(root string) ([]*Manifest, error) {
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: read %s: %w", root, err)
	}
	var out []*Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "plugin.json")
		m, err := LoadManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plugin: skip %s: %v\n", path, err)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// InProc returns the registered in-process plugins. The returned slice is
// shared with the registry; callers must not mutate it.
func (r *Registry) InProc() []Plugin {
	return r.inProc
}

// Manifests returns the discovered out-of-process manifests. The returned
// slice is shared with the registry; callers must not mutate it.
func (r *Registry) Manifests() []*Manifest {
	return r.manifests
}

// ByName looks up a plugin by name. In-process plugins take precedence over
// manifests with the same name.
func (r *Registry) ByName(name string) (Plugin, *Manifest, bool) {
	for _, p := range r.inProc {
		if p.Name() == name {
			return p, nil, true
		}
	}
	for _, m := range r.manifests {
		if m.Name == name {
			return nil, m, true
		}
	}
	return nil, nil, false
}
