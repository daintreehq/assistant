package cli

import (
	"testing"

	"github.com/daintreehq/assistant/internal/mcpserver"
)

// mcpserve_test.go pins the two-layer approval decision, which is easy to get subtly
// wrong: Config.AutoApprove makes tools.Dispatch skip the confirm hook ENTIRELY, while
// the broker handles the hook. They must always agree.

// resolveApprovalMode is the decision under test, extracted so it can be asserted
// without standing up a project lease and a backend.
func TestApprovalModeResolution(t *testing.T) {
	cases := []struct {
		name        string
		requested   mcpserver.ApprovalMode
		defaultAuto bool
		wantMode    mcpserver.ApprovalMode
		wantAuto    bool
	}{
		{
			name: "explicit ask never auto-approves",
			// The dangerous combination: if this resolved auto=true, dispatch would skip
			// the hook and every mutating call would run WITHOUT being asked about.
			requested: mcpserver.ApprovalAsk, defaultAuto: true,
			wantMode: mcpserver.ApprovalAsk, wantAuto: false,
		},
		{
			name:      "explicit decline beats an auto-approving environment",
			requested: mcpserver.ApprovalDecline, defaultAuto: true,
			wantMode: mcpserver.ApprovalDecline, wantAuto: false,
		},
		{
			name:      "explicit auto sets both layers",
			requested: mcpserver.ApprovalAuto, defaultAuto: false,
			wantMode: mcpserver.ApprovalAuto, wantAuto: true,
		},
		{
			// The bug this replaced: an unset mode read the Options pointer, which is nil
			// when only the ENV set auto-approve, so it silently resolved to decline and
			// then wrote an explicit false that suppressed the environment.
			name:      "unset mode inherits an auto-approving environment",
			requested: "", defaultAuto: true,
			wantMode: mcpserver.ApprovalAuto, wantAuto: true,
		},
		{
			name:      "unset mode defaults to the safe one",
			requested: "", defaultAuto: false,
			wantMode: mcpserver.ApprovalDecline, wantAuto: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, auto := resolveApprovalMode(tc.requested, tc.defaultAuto)
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if auto != tc.wantAuto {
				t.Errorf("autoApprove = %v, want %v", auto, tc.wantAuto)
			}
			// The invariant that ties the two layers: dispatch may skip the hook only
			// when the mode says nobody should be asked.
			if auto != (mode == mcpserver.ApprovalAuto) {
				t.Errorf("the two layers disagree: mode=%q autoApprove=%v", mode, auto)
			}
		})
	}
}
