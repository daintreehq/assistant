package host

import (
	"fmt"
	"testing"
)

func TestSeverityForResult(t *testing.T) {
	cases := map[AuditResult]AuditSeverity{
		AuditSuccess:             SeverityInfo,
		AuditDedup:               SeverityInfo,
		AuditConfirmationPending: SeverityNotice,
		AuditUnauthorized:        SeverityWarning,
		AuditRateLimited:         SeverityWarning,
		AuditCollision:           SeverityWarning,
		AuditError:               SeverityErrorSev,
	}
	for res, want := range cases {
		if got := SeverityForResult(res); got != want {
			t.Errorf("SeverityForResult(%q)=%q want %q", res, got, want)
		}
		if want == SeverityCritical {
			t.Errorf("map produced critical for %q — it never should", res)
		}
	}
	// Unknown → error (the const-map undefined fallthrough).
	if got := SeverityForResult("bogus"); got != SeverityErrorSev {
		t.Errorf("unknown severity=%q want error", got)
	}
}

func TestParseDescriptor(t *testing.T) {
	good := `{"sessionId":"s1","windowId":7,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	d, err := ParseDescriptor([]byte(good))
	if err != nil {
		t.Fatalf("good descriptor errored: %v", err)
	}
	if d.SessionID != "s1" || d.WindowID != 7 || d.ProtocolVersion != ProtocolVersion {
		t.Fatalf("bad parse: %+v", d)
	}

	// Missing a required field → error.
	bad := []string{
		`{"windowId":7,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`,              // no sessionId
		`{"sessionId":"s","projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`,           // no windowId
		`{"sessionId":"s","windowId":"7","projectId":"p","cwd":"/x","tier":"s","protocolVersion":4}`, // windowId string
		`not json`,
	}
	for _, b := range bad {
		if _, err := ParseDescriptor([]byte(b)); err == nil {
			t.Errorf("expected error for %q", b)
		}
	}

	// resumeSessionId optional + echoed.
	r, err := ParseDescriptor([]byte(`{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4,"resumeSessionId":"old"}`))
	if err != nil || r.ResumeSessionID != "old" {
		t.Fatalf("resume parse: %+v err=%v", r, err)
	}
}

func TestParseCommand(t *testing.T) {
	ok := map[string]HostCommandType{
		`{"type":"prompt","sessionId":"s","text":"hi"}`:                                     CmdPrompt,
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":"approved"}`: CmdApprovalDecide,
		`{"type":"interrupt","sessionId":"s"}`:                                              CmdInterrupt,
		`{"type":"hibernate","sessionId":"s"}`:                                              CmdHibernate,
		`{"type":"shutdown","sessionId":"s"}`:                                               CmdShutdown,
	}
	for line, wantType := range ok {
		c, err := ParseCommand([]byte(line))
		if err != nil || c.Type != wantType {
			t.Errorf("ParseCommand(%q)=%+v err=%v want type %q", line, c, err, wantType)
		}
	}

	drop := []string{
		`{"type":"prompt","sessionId":"s"}`,          // missing text
		`{"type":"approval:decide","sessionId":"s"}`, // missing approvalId/decision
		`{"type":"unknown","sessionId":"s"}`,         // unknown type
		`{"type":"prompt"}`,                          // missing sessionId
		`garbage`,
	}
	for _, line := range drop {
		if _, err := ParseCommand([]byte(line)); err == nil {
			t.Errorf("expected drop for %q", line)
		}
	}
}

// Fix 5: a fractional protocolVersion must NOT truncate to its floor and slip past
// the == ProtocolVersion check — it is a real mismatch / malformed peer.
//
// Both fixtures are built from ProtocolVersion rather than written as literals, so
// the next protocol bump cannot leave this test asserting against a version the
// engine has stopped accepting (which is how it read after the v3 → v4 bump).
func TestParseDescriptorRejectsFractionalProtocolVersion(t *testing.T) {
	desc := func(v string) []byte {
		return []byte(`{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":` + v + `}`)
	}
	fractional := fmt.Sprintf("%d.9", ProtocolVersion)
	if _, err := ParseDescriptor(desc(fractional)); err == nil {
		t.Fatalf("fractional protocolVersion %s must be rejected, not truncated", fractional)
	}
	// A whole float (N.0) is still accepted (equals its integer).
	d, err := ParseDescriptor(desc(fmt.Sprintf("%d.0", ProtocolVersion)))
	if err != nil || d.ProtocolVersion != ProtocolVersion {
		t.Fatalf("%d.0 should parse to %d: %+v err=%v", ProtocolVersion, ProtocolVersion, d, err)
	}
}

// Fix 8: an unknown decision must normalize to the safe default (rejected), never
// be echoed back verbatim as an off-contract approval:decided.
func TestParseCommandNormalizesUnknownDecision(t *testing.T) {
	cases := map[string]string{
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":"wat"}`:      "rejected",
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":""}`:         "rejected",
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":"approved"}`: "approved",
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":"rejected"}`: "rejected",
		`{"type":"approval:decide","sessionId":"s","approvalId":"a","decision":"timeout"}`:  "timeout",
	}
	for line, want := range cases {
		c, err := ParseCommand([]byte(line))
		if err != nil {
			t.Fatalf("ParseCommand(%q) errored: %v", line, err)
		}
		if c.Decision != want {
			t.Errorf("ParseCommand(%q) decision=%q want %q", line, c.Decision, want)
		}
	}
}

// A question:answer command must parse only when it is complete and well-typed. The
// choiceIndex check matters most: a missing or non-numeric one would default to 0 and
// silently answer "the first option" for a user who never chose.
func TestParseQuestionAnswerCommand(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		idx  int
	}{
		{"valid", `{"sessionId":"s1","type":"question:answer","questionId":"qst_1","choiceIndex":2}`, true, 2},
		{"dismissal", `{"sessionId":"s1","type":"question:answer","questionId":"qst_1","choiceIndex":-1}`, true, -1},
		{"missing index", `{"sessionId":"s1","type":"question:answer","questionId":"qst_1"}`, false, 0},
		{"quoted index", `{"sessionId":"s1","type":"question:answer","questionId":"qst_1","choiceIndex":"2"}`, false, 0},
		{"fractional index", `{"sessionId":"s1","type":"question:answer","questionId":"qst_1","choiceIndex":1.5}`, false, 0},
		{"missing questionId", `{"sessionId":"s1","type":"question:answer","choiceIndex":0}`, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, err := ParseCommand([]byte(c.line))
			if c.ok {
				if err != nil {
					t.Fatalf("ParseCommand: %v", err)
				}
				if cmd.Type != CmdQuestionAnswer {
					t.Fatalf("type = %q, want question:answer", cmd.Type)
				}
				if cmd.ChoiceIndex != c.idx {
					t.Fatalf("choiceIndex = %d, want %d", cmd.ChoiceIndex, c.idx)
				}
				return
			}
			if err == nil {
				t.Fatalf("a malformed question:answer parsed as %+v; it must be dropped", cmd)
			}
		})
	}
}
