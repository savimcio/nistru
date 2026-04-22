package config

import (
	"os"
	"testing"
)

// unsetEnv unsets key for the duration of the test. t.Setenv only supports
// "set to empty"; for our applyEnv logic we need to exercise the false
// branch of os.LookupEnv, which requires an actual Unsetenv.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
