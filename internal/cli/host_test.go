package cli

import (
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/host"
)

// The descriptor is what Daintree BELIEVES it opened; the environment is what this
// runtime actually binds to. Nothing compared them, so the two could disagree while both
// sides reported success — Daintree rendering a conversation as one project's while the
// runtime spawned agents in another's.
func TestDescriptorBindingIsCrossCheckedAgainstTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg := config.AppConfig{
		ProjectID:   "proj_alpha",
		WindowID:    "7",
		Tier:        domain.TierSystem,
		ProjectPath: dir,
	}

	// Agreement passes.
	agree := host.Binding{ProjectID: "proj_alpha", WindowID: "7", Tier: "system", Cwd: dir}
	if err := checkDescriptorBinding(agree, cfg); err != nil {
		t.Fatalf("a matching descriptor was refused: %v", err)
	}

	// Each field disagreeing is refused, and the error names which one.
	for _, tc := range []struct {
		field    string
		declared host.Binding
	}{
		{"projectId", host.Binding{ProjectID: "proj_beta", WindowID: "7", Tier: "system", Cwd: dir}},
		{"windowId", host.Binding{ProjectID: "proj_alpha", WindowID: "9", Tier: "system", Cwd: dir}},
		{"tier", host.Binding{ProjectID: "proj_alpha", WindowID: "7", Tier: "supervisor", Cwd: dir}},
	} {
		err := checkDescriptorBinding(tc.declared, cfg)
		var mismatch *host.BindingMismatchError
		if !errors.As(err, &mismatch) {
			t.Errorf("a %s disagreement was accepted: %v", tc.field, err)
			continue
		}
		if mismatch.Field != tc.field {
			t.Errorf("the mismatch named %q, want %q", mismatch.Field, tc.field)
		}
	}

	// A field neither side states is not a disagreement — the descriptor validates
	// these for type and the live values come from the environment, so absent means
	// "not stated" rather than "stated as empty". Comparing an unstated field would
	// refuse every launch that simply does not inject that variable.
	partial := host.Binding{ProjectID: "proj_alpha"}
	if err := checkDescriptorBinding(partial, cfg); err != nil {
		t.Errorf("an unstated field was treated as a disagreement: %v", err)
	}
	if err := checkDescriptorBinding(agree, config.AppConfig{ProjectPath: dir}); err != nil {
		t.Errorf("an environment that states nothing was treated as a disagreement: %v", err)
	}
}

// cwd is deliberately NOT cross-checked, and this pins that as a decision rather than an
// omission: hostOverrides derives the config's project path FROM the descriptor's cwd, so
// comparing them would compare a value against itself. A check that can never fire is
// worse than no check — it makes the cross-check look more complete than it is.
func TestBindingDoesNotPretendToCrossCheckCwd(t *testing.T) {
	cfg := config.AppConfig{ProjectPath: t.TempDir()}
	elsewhere := t.TempDir()
	if err := checkDescriptorBinding(host.Binding{Cwd: elsewhere}, cfg); err != nil {
		t.Errorf("cwd was compared against a value derived from it: %v", err)
	}
}
