//go:build unix

package supervisor

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// envValues collects every value env holds for name, in order. The daemon's
// config sees the LAST one, but any inherited copy surviving at all is the bug.
func envValues(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

var inheritedCredentials = []string{
	"PATH=/usr/bin",
	"DAINTREE_API_KEY=sk-test-fakeinheritedkey1234567890",
	"DAINTREE_UPSTREAM_PROVIDER=openrouter",
	"DAINTREE_UPSTREAM_MODEL=fake/model",
	"DAINTREE_UPSTREAM_API_KEY=sk-test-fakeinheritedupstream",
	"DAINTREE_UPSTREAM_SORT=price",
	"DAINTREE_UPSTREAM_DATA_COLLECTION=deny",
	"DAINTREE_UPSTREAM_ZDR=true",
}

// A launch whose config DROPPED the inherited credentials (an `mcp --stdio` session
// that redirected the backend) must hand the daemon none of them: the daemon
// re-resolves from its env and would otherwise send them to the redirected URL.
func TestDaemonEnvDoesNotResurrectDroppedCredentials(t *testing.T) {
	cfg := config.AppConfig{StateDir: "/tmp/state", BackendURL: "http://redirected.example"}
	env := daemonEnv(cfg, inheritedCredentials)
	for _, kv := range env {
		if strings.HasPrefix(kv, "DAINTREE_UPSTREAM_") || strings.HasPrefix(kv, "DAINTREE_API_KEY=") {
			t.Errorf("dropped credential reached the daemon: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if got := envValues(env, "PATH"); len(got) != 1 || got[0] != "/usr/bin" {
		t.Errorf("unrelated inherited env was not kept: PATH=%v", got)
	}
}

// The normal case still propagates them — wake turns need the provider key — and
// only the RESOLVED values cross, once each.
func TestDaemonEnvCarriesResolvedCredentials(t *testing.T) {
	cfg := config.AppConfig{
		StateDir: "/tmp/state",
		APIKey:   "sk-test-fakeresolvedkey1234567890",
		Upstream: backend.Upstream{
			Provider: "openrouter", Model: "fake/model", APIKey: "sk-test-fakeresolvedupstream",
			Sort: "price", DataCollection: "deny", ZDR: "true",
		},
	}
	env := daemonEnv(cfg, inheritedCredentials)
	for name, want := range map[string]string{
		"DAINTREE_API_KEY":                  cfg.APIKey,
		"DAINTREE_UPSTREAM_PROVIDER":        "openrouter",
		"DAINTREE_UPSTREAM_MODEL":           "fake/model",
		"DAINTREE_UPSTREAM_API_KEY":         "sk-test-fakeresolvedupstream",
		"DAINTREE_UPSTREAM_SORT":            "price",
		"DAINTREE_UPSTREAM_DATA_COLLECTION": "deny",
		"DAINTREE_UPSTREAM_ZDR":             "true",
	} {
		if got := envValues(env, name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want exactly [%s]", name, got, want)
		}
	}
}
