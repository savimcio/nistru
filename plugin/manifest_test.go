package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// longName yields a name of the given length composed of 'a's — useful for
// building boundary cases around the 64-character upper bound.
func longName(n int) string {
	return strings.Repeat("a", n)
}

func TestLoadManifest(t *testing.T) {
	type want struct {
		err      bool
		errNeeds []string // substrings that must appear in the error message
	}
	cases := []struct {
		desc     string
		contents string
		want     want
		check    func(t *testing.T, m *Manifest)
	}{
		{
			desc: "happy path",
			contents: `{
				"name": "hello-world",
				"version": "0.1.0",
				"cmd": ["node", "index.js"],
				"activation": ["onStart", "onLanguage:go"],
				"capabilities": ["commands"]
			}`,
			want: want{err: false},
			check: func(t *testing.T, m *Manifest) {
				if m.Name != "hello-world" {
					t.Fatalf("Name = %q, want hello-world", m.Name)
				}
				if m.Version != "0.1.0" {
					t.Fatalf("Version = %q, want 0.1.0", m.Version)
				}
				if len(m.Cmd) != 2 || m.Cmd[0] != "node" || m.Cmd[1] != "index.js" {
					t.Fatalf("Cmd = %v, want [node index.js]", m.Cmd)
				}
				if len(m.Activation) != 2 {
					t.Fatalf("Activation = %v, want two entries", m.Activation)
				}
				if len(m.Capabilities) != 1 || m.Capabilities[0] != "commands" {
					t.Fatalf("Capabilities = %v, want [commands]", m.Capabilities)
				}
			},
		},
		{
			desc:     "empty name",
			contents: `{"name": "", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "uppercase name",
			contents: `{"name": "HelloWorld", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "leading hyphen",
			contents: `{"name": "-bad", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "too long (>64 chars)",
			contents: `{"name": "` + longName(65) + `", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "exactly 64 chars is OK",
			contents: `{"name": "` + longName(64) + `", "cmd": ["x"]}`,
			want:     want{err: false},
		},
		{
			desc:     "non-ascii name",
			contents: `{"name": "héllo", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "name with slash",
			contents: `{"name": "foo/bar", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "name with backslash",
			contents: `{"name": "foo\\bar", "cmd": ["x"]}`,
			want:     want{err: true, errNeeds: []string{"name"}},
		},
		{
			desc:     "empty cmd",
			contents: `{"name": "ok", "cmd": []}`,
			want:     want{err: true, errNeeds: []string{"cmd"}},
		},
		{
			desc:     "missing cmd",
			contents: `{"name": "ok"}`,
			want:     want{err: true, errNeeds: []string{"cmd"}},
		},
		{
			desc:     "malformed json",
			contents: `{"name": "ok", "cmd":`,
			want:     want{err: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "plugin.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			m, err := LoadManifest(path)
			if tc.want.err {
				if err == nil {
					t.Fatalf("LoadManifest: expected error, got nil")
				}
				for _, needle := range tc.want.errNeeds {
					if !strings.Contains(err.Error(), needle) {
						t.Fatalf("LoadManifest: error %q missing substring %q", err.Error(), needle)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadManifest: unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	_, err := LoadManifest(path)
	if err == nil {
		t.Fatalf("LoadManifest: expected error on missing file")
	}
}
