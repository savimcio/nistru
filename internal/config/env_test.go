package config

import (
	"testing"
)

func newCfgForEnv() *Config {
	c := Defaults()
	// Empty plugin overlays so tests see only what applyEnv wrote.
	c.Plugins.EnvOverlay = map[string]map[string]any{}
	return c
}

func TestEnvRepo(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "foo/bar")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "")
	c := newCfgForEnv()
	if ws := applyEnv(c); len(ws) != 0 {
		t.Errorf("unexpected warnings: %+v", ws)
	}
	if c.Plugins.EnvOverlay["autoupdate"]["repo"] != "foo/bar" {
		t.Errorf("repo overlay = %v", c.Plugins.EnvOverlay["autoupdate"])
	}
}

func TestEnvChannel(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "dev")
	c := newCfgForEnv()
	applyEnv(c)
	if c.Plugins.EnvOverlay["autoupdate"]["channel"] != "dev" {
		t.Errorf("channel overlay = %v", c.Plugins.EnvOverlay["autoupdate"])
	}
}

func TestEnvInterval(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "30m")
	c := newCfgForEnv()
	ws := applyEnv(c)
	if len(ws) != 0 {
		t.Errorf("warnings: %+v", ws)
	}
	got := c.Plugins.EnvOverlay["autoupdate"]["interval"]
	if got != "30m0s" && got != "30m" {
		t.Errorf("interval = %v", got)
	}
}

func TestEnvIntervalBadValue(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "not-a-duration")
	c := newCfgForEnv()
	ws := applyEnv(c)
	if len(ws) == 0 {
		t.Error("expected a warning for bad interval")
	}
	if _, present := c.Plugins.EnvOverlay["autoupdate"]["interval"]; present {
		t.Errorf("bad interval should not be written")
	}
}

func TestEnvDisableTrueVariants(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "YES"} {
		t.Setenv("NISTRU_AUTOUPDATE_DISABLE", v)
		c := newCfgForEnv()
		ws := applyEnv(c)
		if len(ws) != 0 {
			t.Errorf("warnings for %q: %+v", v, ws)
		}
		if c.Plugins.EnvOverlay["autoupdate"]["disable"] != true {
			t.Errorf("DISABLE=%q didn't set true: %v", v, c.Plugins.EnvOverlay["autoupdate"])
		}
	}
}

func TestEnvDisableFalseVariants(t *testing.T) {
	for _, v := range []string{"0", "false", "no"} {
		t.Setenv("NISTRU_AUTOUPDATE_DISABLE", v)
		c := newCfgForEnv()
		applyEnv(c)
		if c.Plugins.EnvOverlay["autoupdate"]["disable"] != false {
			t.Errorf("DISABLE=%q didn't set false: %v", v, c.Plugins.EnvOverlay["autoupdate"])
		}
	}
}

func TestEnvDisableEmptyIsNoOp(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "")
	c := newCfgForEnv()
	applyEnv(c)
	if _, present := c.Plugins.EnvOverlay["autoupdate"]["disable"]; present {
		t.Error("empty DISABLE should be a no-op")
	}
}

func TestEnvDisableUnknownWarns(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "maybe")
	c := newCfgForEnv()
	ws := applyEnv(c)
	if len(ws) == 0 {
		t.Error("expected warning for unrecognized bool")
	}
}

func TestEnvAllUnsetIsNoOp(t *testing.T) {
	for _, k := range []string{
		"NISTRU_AUTOUPDATE_REPO",
		"NISTRU_AUTOUPDATE_CHANNEL",
		"NISTRU_AUTOUPDATE_INTERVAL",
		"NISTRU_AUTOUPDATE_DISABLE",
	} {
		t.Setenv(k, "")
		// t.Setenv to "" still counts as set; explicitly unset below.
	}
	// Force-unset so lookup returns (_, false).
	for _, k := range []string{
		"NISTRU_AUTOUPDATE_REPO",
		"NISTRU_AUTOUPDATE_CHANNEL",
		"NISTRU_AUTOUPDATE_INTERVAL",
		"NISTRU_AUTOUPDATE_DISABLE",
	} {
		// Use Setenv('') then Unsetenv via os
		unsetEnv(t, k)
	}
	c := newCfgForEnv()
	ws := applyEnv(c)
	if len(ws) != 0 {
		t.Errorf("warnings: %+v", ws)
	}
	if _, present := c.Plugins.EnvOverlay["autoupdate"]; present {
		t.Errorf("overlay should be empty: %v", c.Plugins.EnvOverlay["autoupdate"])
	}
}
