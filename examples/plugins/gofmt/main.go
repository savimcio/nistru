// Command gofmt-plugin is an nistru out-of-process plugin that formats Go
// buffers by shelling out to the system `gofmt`. It re-formats on save
// automatically and exposes a "gofmt" command for on-demand formatting.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/savimcio/nistru/sdk/plugsdk"
)

// gofmtPlugin tracks the latest known text for each open path. The SDK
// invokes every handler on a single reader goroutine, so contention is not
// the concern; the mutex just makes the shared-state access pattern
// explicit.
type gofmtPlugin struct {
	plugsdk.Base

	mu          sync.Mutex
	texts       map[string]string
	currentPath string
}

// OnInitialize registers the on-demand "gofmt" command. Auto-format-on-save
// is driven by OnDidSave and needs no explicit registration.
func (p *gofmtPlugin) OnInitialize(root string, capabilities []string) error {
	p.texts = make(map[string]string)
	return p.Client().RegisterCommand("gofmt", "Format with gofmt")
}

// OnDidOpen records the initial text and marks the opened file as current.
func (p *gofmtPlugin) OnDidOpen(path, lang, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.texts[path] = text
	p.currentPath = path
}

// OnDidChange keeps the cached text in sync with the host's buffer.
func (p *gofmtPlugin) OnDidChange(path, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.texts[path] = text
	p.currentPath = path
}

// OnDidClose drops the cached text and clears currentPath if the closed
// file was current.
func (p *gofmtPlugin) OnDidClose(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.texts, path)
	if p.currentPath == path {
		p.currentPath = ""
	}
}

// OnDidSave formats-on-save for *.go paths. Errors are logged to stderr;
// notifications are not the save path's job.
func (p *gofmtPlugin) OnDidSave(path string) {
	if filepath.Ext(path) != ".go" {
		return
	}
	if err := p.formatPath(path); err != nil {
		fmt.Fprintf(os.Stderr, "gofmt: didSave %s: %v\n", path, err)
	}
}

// OnExecuteCommand runs gofmt over the current buffer when asked. Any id
// other than "gofmt" falls through to the embedded Base default.
func (p *gofmtPlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	if id != "gofmt" {
		return p.Base.OnExecuteCommand(id, args)
	}
	p.mu.Lock()
	path := p.currentPath
	p.mu.Unlock()
	if path == "" {
		return nil, fmt.Errorf("gofmt: no current file")
	}
	if err := p.formatPath(path); err != nil {
		return nil, err
	}
	return nil, nil
}

// formatPath fetches the latest cached text for path, runs gofmt over it,
// and asks the host to replace the buffer on success.
func (p *gofmtPlugin) formatPath(path string) error {
	p.mu.Lock()
	text, ok := p.texts[path]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("gofmt: no cached text for %s", path)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("gofmt")
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gofmt: %s", strings.TrimSpace(stderr.String()))
	}

	formatted := stdout.String()
	if formatted == text {
		return nil
	}

	p.mu.Lock()
	p.texts[path] = formatted
	p.mu.Unlock()

	return p.Client().BufferEdit(path, formatted)
}

func main() {
	if err := plugsdk.Run(&gofmtPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "gofmt-plugin: %v\n", err)
		os.Exit(1)
	}
}
