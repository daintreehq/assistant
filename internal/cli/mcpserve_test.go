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

// TestSessionProjectIdentityOverridesProcessDefaults pins the defaults-not-bindings
// rule for the identity pair: the launch config SEEDS a session, it does not constrain
// one, and an omitted session field must not blank out what the process was launched
// with. It also pins that a process-level --prompt-file never leaks into a session — on
// the stdio transport, "-" would be read off the JSON-RPC stream itself.
func TestSessionProjectIdentityOverridesProcessDefaults(t *testing.T) {
	process := Options{
		ProjectID:  "launch-project",
		WindowID:   "launch-window",
		Prompt:     "a launch prompt",
		HasPrompt:  true,
		PromptFile: "-",
		MultiTurn:  true,
	}

	// Both fields, both directions — a table so deleting EITHER overlay line fails.
	for _, tc := range []struct {
		name                 string
		params               mcpserver.OpenParams
		wantProject, wantWin string
	}{
		{"project overrides, window inherits",
			mcpserver.OpenParams{ProjectID: "session-project"}, "session-project", "launch-window"},
		{"window overrides, project inherits",
			mcpserver.OpenParams{WindowID: "session-window"}, "launch-project", "session-window"},
		{"both override",
			mcpserver.OpenParams{ProjectID: "p2", WindowID: "w2"}, "p2", "w2"},
		{"neither given inherits both",
			mcpserver.OpenParams{}, "launch-project", "launch-window"},
		// Whitespace is NOT a value. config's FirstString trims what it resolves, so a
		// raw " " stored here would count as set at this layer and unset at that one:
		// the launch flag would be discarded and the environment (or a bare state root)
		// would answer instead — silently opening the wrong project's database.
		{"whitespace is treated as omitted",
			mcpserver.OpenParams{ProjectID: "   ", WindowID: "\t"}, "launch-project", "launch-window"},
		// Blankness decides whether an argument was GIVEN; it does not rewrite what one
		// says. Several overlaid fields are paths, where a trailing space is a legal part
		// of a filename — reading "/keys/account" because the caller named
		// "/keys/account " would bill a different credential.
		{"a padded value is preserved verbatim",
			mcpserver.OpenParams{ProjectID: "  padded  "}, "  padded  ", "launch-window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := sessionOptions(process, tc.params)
			if session.ProjectID != tc.wantProject {
				t.Errorf("ProjectID = %q, want %q", session.ProjectID, tc.wantProject)
			}
			if session.WindowID != tc.wantWin {
				t.Errorf("WindowID = %q, want %q", session.WindowID, tc.wantWin)
			}
			// MultiTurn is in this set for the sharpest version of the same reason: it
			// reads prompts from stdin line by line, which on the stdio transport is the
			// JSON-RPC stream carrying the protocol itself.
			if session.Prompt != "" || session.HasPrompt || session.PromptFile != "" || session.MultiTurn {
				t.Errorf("one-shot prompt state leaked into a session: prompt=%q hasPrompt=%v promptFile=%q multiTurn=%v",
					session.Prompt, session.HasPrompt, session.PromptFile, session.MultiTurn)
			}
		})
	}
}

// The nil-versus-empty distinction is a real instruction, not pedantry: a caller sending
// `"skills": []` on session.open is explicitly clearing whatever `--skill` this server
// process was launched with, and length-testing would silently reverse that into
// "inherit them" — pinning a session to runbooks it asked not to have.
func TestApplySliceIfSet(t *testing.T) {
	defaults := []string{"proc.default"}

	t.Run("nil inherits the process default", func(t *testing.T) {
		dst := append([]string(nil), defaults...)
		applySliceIfSet(&dst, nil)
		if len(dst) != 1 || dst[0] != "proc.default" {
			t.Fatalf("dst = %v, want the process default preserved", dst)
		}
	})

	t.Run("non-nil empty CLEARS the process default", func(t *testing.T) {
		dst := append([]string(nil), defaults...)
		applySliceIfSet(&dst, []string{})
		if len(dst) != 0 {
			t.Fatalf("dst = %v, want an explicit empty array to clear the default", dst)
		}
	})

	t.Run("non-empty replaces", func(t *testing.T) {
		dst := append([]string(nil), defaults...)
		applySliceIfSet(&dst, []string{"a.one", "b.two"})
		if len(dst) != 2 || dst[0] != "a.one" || dst[1] != "b.two" {
			t.Fatalf("dst = %v, want the session's own list in order", dst)
		}
	})

	// The decoded session argument must not stay aliased into the process-level options
	// that seed EVERY later session — one caller's pins would leak into the next.
	t.Run("copies defensively", func(t *testing.T) {
		src := []string{"a.one"}
		var dst []string
		applySliceIfSet(&dst, src)
		src[0] = "mutated"
		if dst[0] != "a.one" {
			t.Fatalf("dst aliased the source: %v", dst)
		}
	})
}

// TestSessionOptionsOverlaysPinnedSkills pins the WIRING, which TestApplySliceIfSet
// above deliberately cannot: that one proves the helper behaves, this one proves
// sessionOptions actually calls it for the pin field. The two headless surfaces are
// wired through different lines, and a dropped overlay here fails in the quietest way
// available — the session simply runs with the process default (or with nothing),
// producing a green test suite and a run pinned to the wrong runbooks.
func TestSessionOptionsOverlaysPinnedSkills(t *testing.T) {
	process := Options{PinnedSkillIDs: []string{"proc.default"}}

	for _, tc := range []struct {
		name  string
		given []string
		want  []string
	}{
		// Omitted is not "cleared": a session that said nothing inherits what the
		// server process was launched with.
		{"omitted inherits the launch pins", nil, []string{"proc.default"}},
		// An explicit empty array is an instruction, and the opposite one.
		{"an explicit empty array clears them", []string{}, []string{}},
		{"a list replaces them in order", []string{"b.two", "a.one"}, []string{"b.two", "a.one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionOptions(process, mcpserver.OpenParams{Skills: tc.given}).PinnedSkillIDs
			if len(got) != len(tc.want) {
				t.Fatalf("PinnedSkillIDs = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("PinnedSkillIDs = %v, want %v", got, tc.want)
				}
			}
		})
	}

	// The overlay must not write through into the options that seed every later
	// session — one caller's pins leaking into the next is the failure this guards.
	if len(process.PinnedSkillIDs) != 1 || process.PinnedSkillIDs[0] != "proc.default" {
		t.Fatalf("sessionOptions mutated the process-level pins: %v", process.PinnedSkillIDs)
	}
}
