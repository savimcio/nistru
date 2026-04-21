package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// manifestNameRe bounds the set of legal plugin names. It is compiled once at
// package scope so LoadManifest is cheap to call repeatedly.
var manifestNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Manifest describes an out-of-process plugin as declared by its plugin.json.
type Manifest struct {
	// Name is the stable plugin identifier; must match ^[a-z0-9][a-z0-9-]{0,63}$.
	Name string `json:"name"`
	// Version is a free-form version string reported for logging.
	Version string `json:"version"`
	// Cmd is the argv the host will spawn (argv[0] is the executable).
	Cmd []string `json:"cmd"`
	// Activation lists activation event patterns (see activation.go).
	Activation []string `json:"activation"`
	// Capabilities advertises which host extension points the plugin uses.
	Capabilities []string `json:"capabilities"`
}

// LoadManifest reads and validates a plugin.json file at path.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: decode %s: %w", path, err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("manifest: %s: name is required", path)
	}
	if !manifestNameRe.MatchString(m.Name) {
		return nil, fmt.Errorf("manifest: %s: name %q must match %s", path, m.Name, manifestNameRe)
	}
	if len(m.Cmd) == 0 {
		return nil, fmt.Errorf("manifest: %s: cmd must be non-empty", path)
	}
	return &m, nil
}
