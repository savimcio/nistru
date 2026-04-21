package autoupdate

import (
	"context"

	"github.com/savimcio/nistru/plugin"
)

// noopInstaller is the placeholder Installer used until T7 wires the real
// binary-swap path. It surfaces a one-line warning through PostNotif so the
// user sees the command dispatch succeeded even though nothing happens yet.
type noopInstaller struct{}

// Install implements Installer. The noop variant posts a "not yet wired"
// warning and returns nil so the palette command never appears to fail.
func (noopInstaller) Install(ctx context.Context, host *plugin.Host, rel Release, cur string) error {
	_ = ctx
	_ = rel
	_ = cur
	if host != nil {
		_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
			"level":   "warn",
			"message": "auto-update install is not yet wired (T7 pending)",
		})
	}
	return nil
}

// Rollback implements Installer. Same noop semantics as Install.
func (noopInstaller) Rollback(ctx context.Context, host *plugin.Host) error {
	_ = ctx
	if host != nil {
		_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
			"level":   "warn",
			"message": "auto-update rollback is not yet wired (T7 pending)",
		})
	}
	return nil
}

// Compile-time assertion that noopInstaller satisfies Installer. Kept so the
// seam remains a drop-in replacement for tests that want to bypass the real
// installer, and so staticcheck does not flag the type as unused now that
// New() defaults to NewInstaller().
var _ Installer = noopInstaller{}
