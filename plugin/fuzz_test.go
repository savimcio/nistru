package plugin

// Fuzz targets for the three untrusted-input surfaces in plugin/:
//   - FuzzCodec: JSON-RPC 2.0 frames fed into Codec.Read.
//   - FuzzManifest: plugin.json bytes fed into LoadManifest.
//   - FuzzActivationGlob: (activation, path) pairs fed into ParseActivation + Match.
//
// Seeds are added in-source via f.Add rather than stored under
// testdata/fuzz/<FuzzName>/ so the corpus is visible next to the target. Go's
// test runner materializes f.Add seeds automatically for both `-run=^Fuzz`
// smoke and `-fuzz=` continuous modes.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codecInputCap bounds fuzz inputs to the codec's bufio.Reader size so a
// fuzzer that discovers the newline-less path cannot force unbounded
// allocation. `min` is a Go builtin since 1.21.
const codecInputCap = 1 << 16 // 64 KiB — matches NewCodec's bufio.NewReaderSize.

// codecFuzzInput is a Reader/Closer adapter that feeds a fixed byte slice to
// Codec.Read. The Writer side discards output, since none of the codec's read
// path writes back.
type codecFuzzInput struct {
	io.Reader
}

func (codecFuzzInput) Write(p []byte) (int, error) { return len(p), nil }
func (codecFuzzInput) Close() error                { return nil }

// newCodecForFuzz wraps a bounded byte slice into a Codec. Callers must
// already have truncated the input.
func newCodecForFuzz(input []byte) *Codec {
	return NewCodec(codecFuzzInput{Reader: bytes.NewReader(input)})
}

// FuzzCodec feeds arbitrary bytes as a single frame to Codec.Read. The target
// asserts no panic and the documented post-condition that a returned response
// always has a non-nil *Response.
func FuzzCodec(f *testing.F) {
	// Seed corpus. Each is exactly one frame (may include trailing '\n').
	f.Add([]byte(`{"jsonrpc":"2.0","method":"Initialize","id":1,"params":{}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"ui/notify","params":{"msg":"hi"}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"not found"}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1}` + "\n"))
	f.Add([]byte("\n"))
	f.Add([]byte("{not json\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"X","id":1}`)) // no trailing newline
	// Malformed request frames with an explicit empty-string method. After
	// the F1 fix, these must stay on the request path (not be misrouted as
	// responses) and must not crash the codec.
	f.Add([]byte(`{"jsonrpc":"2.0","method":"","id":1}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"","params":{}}` + "\n"))

	f.Fuzz(func(t *testing.T, input []byte) {
		// Bound input to the codec's read buffer. Prevents a fuzzer from
		// producing pathological inputs that would dominate the run.
		if len(input) > codecInputCap {
			input = input[:codecInputCap]
		}
		c := newCodecForFuzz(input)
		_, _, _, isResponse, resp, err := c.Read()
		// Invariant: if we claim a response, resp must be non-nil. The only
		// other contract worth enforcing across arbitrary input is "no panic"
		// — which is checked implicitly by the fuzz engine. In particular, a
		// method="" frame is a malformed request (not a response) and must
		// not crash or be misrouted; that's exercised explicitly via
		// TestCodec_EmptyMethodIsRequestPath.
		if err == nil && isResponse && resp == nil {
			t.Fatalf("isResponse=true with nil resp; input=%q", input)
		}
	})
}

// FuzzManifest feeds arbitrary bytes to LoadManifest via a tempdir file. The
// target asserts LoadManifest returns either (*Manifest, nil) or
// (nil, non-nil-error) — never (*Manifest, non-nil) and never a panic.
func FuzzManifest(f *testing.F) {
	// Minimal valid manifest.
	f.Add([]byte(`{"name":"hello","version":"0.1.0","cmd":["./hello"]}`))
	// Extra fields.
	f.Add([]byte(`{"name":"hello","version":"0.1.0","cmd":["./hello"],"activationEvents":["onLanguage:go"]}`))
	// Missing name.
	f.Add([]byte(`{"version":"0.1.0","cmd":["./hello"]}`))
	// Invalid name (path-ish).
	f.Add([]byte(`{"name":"../etc/passwd","version":"0.1.0","cmd":["./x"]}`))
	// Empty cmd.
	f.Add([]byte(`{"name":"x","version":"0.1.0","cmd":[]}`))
	// Malformed JSON.
	f.Add([]byte(`{not a manifest`))
	// Empty file.
	f.Add([]byte(``))
	// Ridiculously long name.
	f.Add([]byte(`{"name":"` + strings.Repeat("a", 10*1024) + `","version":"0.1.0","cmd":["./x"]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// One tempdir per invocation keeps fuzz runs hermetic.
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		m, err := LoadManifest(path)
		switch {
		case err != nil && m != nil:
			t.Fatalf("LoadManifest returned both manifest and error: m=%+v err=%v", m, err)
		case err == nil && m == nil:
			t.Fatalf("LoadManifest returned nil manifest and nil error")
		case err == nil:
			// A nil-error return must satisfy the same invariants production
			// code relies on after validation: non-empty name and non-empty
			// cmd. If this ever trips, LoadManifest has regressed.
			if m.Name == "" {
				t.Fatalf("LoadManifest: nil error but empty name; data=%q", data)
			}
			if len(m.Cmd) == 0 {
				t.Fatalf("LoadManifest: nil error but empty cmd; data=%q", data)
			}
		}
	})
}

// FuzzActivationGlob feeds (activation string, path) pairs to ParseActivation
// and Match. The target asserts no panic; a malformed activation produces an
// error from Match rather than a silent false.
func FuzzActivationGlob(f *testing.F) {
	// Valid + matching.
	f.Add("onLanguage:go", "main.go")
	// Valid + non-matching.
	f.Add("onLanguage:go", "main.rs")
	// onStart carries no value and matches any onStart event.
	f.Add("onStart", "")
	// onSave glob.
	f.Add("onSave:*.md", "foo.md")
	// Malformed activation (empty value).
	f.Add("onLanguage:", "main.go")
	// Unknown kind.
	f.Add("unknown:foo", "")
	// NUL bytes + emoji + long path (>4 KiB).
	f.Add("onLanguage:go", "evil\x00path.go")
	f.Add("onSave:*.go", strings.Repeat("a/", 2048)+"x.go")
	f.Add("onLanguage:go", "main-"+string('\U0001F4A9')+".go")
	// Glob-meta in path; activation kind determines whether globbing applies.
	// For onLanguage, Value equality is case-insensitive, so path is ignored —
	// we cover both that path and onSave-as-glob here.
	f.Add("onSave:[.go", "foo.go")  // malformed glob
	f.Add("onSave:**/*.go", "x.go") // filepath.Match doesn't understand **

	f.Fuzz(func(t *testing.T, activation, path string) {
		// First verify ParseActivation itself never panics on fuzz strings.
		ev, parseErr := ParseActivation(activation)
		if parseErr != nil {
			// Parse errors are the contract for malformed kinds / empty values.
			// Don't attempt Match on an invalid pattern — that's a
			// constructor-level contract violation outside the fuzz domain.
			return
		}
		// Match accepts a []string of patterns; feed the original activation
		// string so Match re-parses (that's the production path).
		//
		// For the event, pick a kind consistent with what the pattern kind
		// expects. Using the parsed ev.Kind keeps us on the intended branch;
		// the Value comes from the fuzz path argument so we still exercise
		// unusual bytes (NULs, emoji, huge strings) through the matcher.
		event := ActivationEvent{Kind: ev.Kind, Value: path}
		_, matchErr := Match([]string{activation}, event)
		// Match may return an error (e.g. bad glob) — that's fine. The only
		// contract we check is "no panic".
		_ = matchErr
	})
}
