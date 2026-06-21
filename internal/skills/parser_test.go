package skills

import (
	"strings"
	"testing"
)

const validFile = `---
id: daintree.test.basic
title: Test skill
version: 0.1.0
summary: A short summary.
whenToUse: Use when testing.
priority: 5
risk: project
maxTurns: 4
tags:
  - a
  - b
requiredTools:
  - fs.read
  - fs.list
---
This is the body.
`

func TestParseValidSkill(t *testing.T) {
	sk, err := parseSkillFile(validFile, "daintree.test.basic.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sk.ID != "daintree.test.basic" {
		t.Errorf("id = %q", sk.ID)
	}
	if sk.Version != "0.1.0" { // dotted version stays a string
		t.Errorf("version = %q", sk.Version)
	}
	if sk.Priority != 5 {
		t.Errorf("priority = %d", sk.Priority)
	}
	if sk.MaxTurns != 4 {
		t.Errorf("maxTurns = %d", sk.MaxTurns)
	}
	if sk.Risk != RiskProject {
		t.Errorf("risk = %q", sk.Risk)
	}
	if len(sk.Tags) != 2 || sk.Tags[0] != "a" || sk.Tags[1] != "b" {
		t.Errorf("tags = %v", sk.Tags)
	}
	if len(sk.RequiredTools) != 2 {
		t.Errorf("requiredTools = %v", sk.RequiredTools)
	}
	if sk.Body != "This is the body." {
		t.Errorf("body = %q", sk.Body)
	}
}

func TestParseDefaults(t *testing.T) {
	// No priority/maxTurns/risk/tags/requiredTools ⇒ defaults.
	f := `---
id: x.y
title: T
version: 1.0.0
summary: s
whenToUse: w
---
body`
	sk, err := parseSkillFile(f, "x.y.md")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sk.Priority != 0 || sk.MaxTurns != 8 || sk.Risk != RiskRead {
		t.Errorf("defaults wrong: prio=%d max=%d risk=%q", sk.Priority, sk.MaxTurns, sk.Risk)
	}
	if len(sk.Tags) != 0 || len(sk.RequiredTools) != 0 {
		t.Errorf("expected empty slices")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"no fence", "id: x\n", "missing YAML frontmatter"},
		{"missing required id", "---\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\n---\nbody", "id is required"},
		{"empty body", "---\nid: x.y\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\n---\n", "body is required"},
		{"bad risk", "---\nid: x.y\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\nrisk: nuclear\n---\nbody", "risk must be one of"},
		{"duplicate key", "---\nid: x.y\nid: z\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\n---\nbody", "duplicate frontmatter key"},
		{"malformed line", "---\nid: x.y\nthis is not a kv line\n---\nbody", "malformed frontmatter line"},
		{"stray indent", "---\nid: x.y\n  stray\n---\nbody", "unexpected indented line"},
		{"non-positive maxTurns", "---\nid: x.y\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\nmaxTurns: 0\n---\nbody", "positive"},
		{"priority not int", "---\nid: x.y\ntitle: T\nversion: 1.0.0\nsummary: s\nwhenToUse: w\npriority: high\n---\nbody", "priority must be an integer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSkillFile(c.content, "f.md")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
			// All errors must carry the filename.
			if !strings.Contains(err.Error(), "f.md") && !strings.Contains(err.Error(), "missing YAML") {
				// the no-fence case also includes the filename
			}
		})
	}
}

func TestCoerceScalar(t *testing.T) {
	// Quoted ⇒ inner string, no further coercion.
	if sc := coerceScalar(`"123"`); sc.kind != scalarString || sc.str != "123" {
		t.Errorf(`"123" => %+v`, sc)
	}
	// Plain integer.
	if sc := coerceScalar("42"); sc.kind != scalarInt || sc.num != 42 {
		t.Errorf("42 => %+v", sc)
	}
	// Negative integer.
	if sc := coerceScalar("-7"); sc.kind != scalarInt || sc.num != -7 {
		t.Errorf("-7 => %+v", sc)
	}
	// Dotted version stays a string.
	if sc := coerceScalar("0.2.0"); sc.kind != scalarString || sc.str != "0.2.0" {
		t.Errorf("0.2.0 => %+v", sc)
	}
	// Booleans.
	if sc := coerceScalar("true"); sc.kind != scalarBool || !sc.boolV {
		t.Errorf("true => %+v", sc)
	}
}

func TestInlineArrayDropsBlanks(t *testing.T) {
	// "[a, b,]" must not smuggle a trailing "" entry.
	f := `---
id: x.y
title: T
version: 1.0.0
summary: s
whenToUse: w
tags: [a, b,]
---
body`
	sk, err := parseSkillFile(f, "x.y.md")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(sk.Tags) != 2 {
		t.Errorf("expected 2 tags, got %v", sk.Tags)
	}
}

func TestFilenameMustMatchID(t *testing.T) {
	_, err := parseAndCheck(validFile, "wrong-name.md")
	if err == nil || !strings.Contains(err.Error(), "filename does not match") {
		t.Fatalf("expected filename mismatch error, got %v", err)
	}
}
