package mcp

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

const wantTerminal = "terminal-aaaa1111"

// A send acknowledgement is parsed AFTER the command has already gone out, so
// the failure modes are asymmetric: refusing a receipt-less ack fails a send
// that really happened, while accepting a malformed one would let a host that
// advertises receipts drift out of contract unnoticed.
func TestSubmissionReceipt(t *testing.T) {
	longToken := strings.Repeat("t", 129)
	cases := []struct {
		name       string
		structured any
		text       string
		wantErr    bool
		acceptance string
		token      string
		phase      string
	}{{
		// The shape EVERY Daintree in the field returns today, and the shape the
		// scripted world (benchmarks/orchestration) and the e2e fake return: a
		// bare ok with no receipt fields at all. It must never fail the send.
		name: "no receipt fields at all is legacy, never a failure",
		text: `{"ok":true}`, acceptance: "legacy_unknown",
	}, {
		name:       "legacy ack that does echo the terminal",
		text:       `{"ok":true,"sent":true,"terminalId":"` + wantTerminal + `"}`,
		acceptance: "legacy_unknown",
	}, {
		name: "empty body is legacy, not a refusal",
		text: `{}`, acceptance: "legacy_unknown",
	}, {
		// Nothing to correlate and nothing to refuse: an unreadable ack is still
		// only "no receipt was offered", and legacy_unknown authorizes no resend.
		name: "unparseable body is legacy, not a refusal",
		text: `not json`, acceptance: "legacy_unknown",
	}, {
		// The one receipt-less ack still worth refusing: the host answered about
		// a DIFFERENT terminal, which is mis-correlation, not an old host.
		name: "receipt-less ack naming another terminal",
		text: `{"ok":true,"terminalId":"terminal-bbbb2222"}`, wantErr: true,
	}, {
		name:       "tracked receipt",
		text:       `{"sent":true,"terminalId":"` + wantTerminal + `","submissionToken":"tok-1"}`,
		acceptance: "tracked", token: "tok-1", phase: "queued",
	}, {
		// Item 2's regression: a host answering structuredContent:{} with the real
		// body in text used to lose the receipt entirely and hard-fail the send.
		name:       "tracked receipt behind an empty structuredContent",
		structured: map[string]any{},
		text:       `{"sent":true,"terminalId":"` + wantTerminal + `","submissionToken":"tok-2"}`,
		acceptance: "tracked", token: "tok-2", phase: "queued",
	}, {
		name:       "structuredContent wins when it carries the receipt",
		structured: map[string]any{"sent": true, "terminalId": wantTerminal, "submissionToken": "tok-3"},
		acceptance: "tracked", token: "tok-3", phase: "queued",
	}, {
		name: "present token for another terminal",
		text: `{"sent":true,"terminalId":"terminal-bbbb2222","submissionToken":"tok-4"}`, wantErr: true,
	}, {
		name: "present token without sent:true",
		text: `{"terminalId":"` + wantTerminal + `","submissionToken":"tok-5"}`, wantErr: true,
	}, {
		name: "present token that is not a string",
		text: `{"sent":true,"terminalId":"` + wantTerminal + `","submissionToken":7}`, wantErr: true,
	}, {
		name: "present token that is empty",
		text: `{"sent":true,"terminalId":"` + wantTerminal + `","submissionToken":""}`, wantErr: true,
	}, {
		name: "present token over the length bound",
		text: `{"sent":true,"terminalId":"` + wantTerminal + `","submissionToken":"` + longToken + `"}`, wantErr: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SubmissionReceipt(tc.structured, tc.text, wantTerminal)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Version != 1 || got.Acceptance != tc.acceptance || got.Token != tc.token || got.Phase != tc.phase {
				t.Fatalf("receipt = %+v, want acceptance=%q token=%q phase=%q", got, tc.acceptance, tc.token, tc.phase)
			}
			if !got.Valid() {
				t.Errorf("parser produced a receipt storage would reject: %+v", got)
			}
		})
	}
}

// The submission record and hasPty ride the same body as the receipt, so the
// text fallback has to reach them too — a structuredContent:{} host would
// otherwise report "no record" and a nil PTY on every read.
func TestReadTerminalSubmissionReadsTheTextBody(t *testing.T) {
	text := `{"terminals":[{"terminalId":"` + wantTerminal + `","hasPty":true,"submission":{"token":"tok-1","phase":"pty_written"}}]}`

	rec, entry, ok := ReadTerminalSubmission(map[string]any{}, text, wantTerminal, "tok-1")
	if !ok || rec.Phase != "pty_written" {
		t.Fatalf("record = %+v ok=%v, want a pty_written record", rec, ok)
	}
	if hasPty := TerminalHasPty(map[string]any{}, text, entry); hasPty == nil || !*hasPty {
		t.Errorf("hasPty = %v, want true", hasPty)
	}

	// A different token on the same terminal is another invocation's send.
	if _, _, ok := ReadTerminalSubmission(nil, text, wantTerminal, "tok-other"); ok {
		t.Error("a foreign token must not resolve")
	}
	// An unrecognized phase is unreadable, never silently coerced.
	bad := `{"terminals":[{"terminalId":"` + wantTerminal + `","submission":{"token":"tok-1","phase":"beamed"}}]}`
	if _, _, ok := ReadTerminalSubmission(nil, bad, wantTerminal, "tok-1"); ok {
		t.Error("an unknown phase must not resolve")
	}
	if !domain.ValidSubmissionPhase("pty_written") || domain.ValidSubmissionPhase("beamed") {
		t.Error("phase vocabulary drifted")
	}
}

// An explicitly unavailable field outranks any value an entry supplies: the
// host is telling us it does not know, and inventing false here would read as
// "the PTY ended".
func TestTerminalHasPtyHonoursUnavailableFields(t *testing.T) {
	entry := map[string]any{"hasPty": false}
	if got := TerminalHasPty(nil, `{"unavailableFields":["hasPty"]}`, entry); got != nil {
		t.Errorf("hasPty = %v, want nil", got)
	}
	if got := TerminalHasPty(nil, `{}`, entry); got == nil || *got {
		t.Errorf("hasPty = %v, want false", got)
	}
	if got := TerminalHasPty(nil, `{}`, map[string]any{}); got != nil {
		t.Errorf("absent hasPty = %v, want nil", got)
	}
}
