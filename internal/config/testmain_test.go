package config

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway dir for the WHOLE package run: every
// LoadConfig reads the global backend-credential file under HOME, so without
// package-scope isolation a developer's real (or corrupt) ~/.daintree/
// credentials.json would leak into any test that doesn't call isolatedHome.
// Individual tests that re-point HOME via t.Setenv still work — t.Setenv
// snapshots and restores this baseline.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "config-test-home")
	if err == nil {
		os.Setenv("HOME", home)
		os.Setenv("USERPROFILE", home)
	}
	code := m.Run()
	if home != "" {
		os.RemoveAll(home)
	}
	os.Exit(code)
}
