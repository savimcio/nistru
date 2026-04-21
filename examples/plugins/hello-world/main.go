// Command hello-world is a minimal nistru out-of-process plugin. On
// initialize it registers a "hello" command; when the host asks to execute
// that command, it replies with a transient status-bar notification.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/savimcio/nistru/sdk/plugsdk"
)

// helloPlugin embeds plugsdk.Base to inherit no-op defaults and the client
// plumbing (SetClient/Client) the SDK expects.
type helloPlugin struct {
	plugsdk.Base
}

// OnInitialize registers the plugin's single command with the host. The
// host will surface it in the command palette keyed by its id.
func (p *helloPlugin) OnInitialize(root string, capabilities []string) error {
	return p.Client().RegisterCommand("hello", "Say Hello")
}

// OnExecuteCommand handles the host's request to run one of our commands.
// For any id other than "hello" we fall back to the embedded Base default.
func (p *helloPlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	if id != "hello" {
		return p.Base.OnExecuteCommand(id, args)
	}
	if err := p.Client().Notify("info", "Hello from plugin!"); err != nil {
		return nil, err
	}
	return nil, nil
}

func main() {
	if err := plugsdk.Run(&helloPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "hello-world: %v\n", err)
		os.Exit(1)
	}
}
