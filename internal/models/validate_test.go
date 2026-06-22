package models

import "testing"

func TestDecodeWatcherVerdict(t *testing.T) {
	// Valid, with defaulting: evidence omitted → [], recommendedAction omitted → none.
	v, err := DecodeWatcherVerdict(`{"classification":"tests_passed","confidence":0.9,"summary":"ok"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Evidence == nil || len(v.Evidence) != 0 {
		t.Fatalf("evidence default = %v, want []", v.Evidence)
	}
	if v.RecommendedAction != "none" {
		t.Fatalf("action default = %q, want none", v.RecommendedAction)
	}

	if _, err := DecodeWatcherVerdict(`{"classification":"bogus","confidence":0.5,"summary":"x"}`); err == nil {
		t.Fatal("invalid classification must error")
	}
	if _, err := DecodeWatcherVerdict(`{"classification":"unknown","confidence":1.5,"summary":"x"}`); err == nil {
		t.Fatal("confidence > 1 must error")
	}
	if _, err := DecodeWatcherVerdict(`{"classification":"unknown","confidence":0.1,"summary":"x","recommendedAction":"nope"}`); err == nil {
		t.Fatal("invalid recommendedAction must error")
	}
}

func TestDecodeModelJudgeAnswer(t *testing.T) {
	a, err := DecodeModelJudgeAnswer(`{"reason":"because","confidence":0.8,"matched":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Matched || a.Reason != "because" {
		t.Fatalf("answer = %+v", a)
	}
	if _, err := DecodeModelJudgeAnswer(`{"confidence":0.5,"matched":true}`); err == nil {
		t.Fatal("missing reason must error")
	}
	if _, err := DecodeModelJudgeAnswer(`{"reason":"x","confidence":2,"matched":false}`); err == nil {
		t.Fatal("confidence out of range must error")
	}
}
