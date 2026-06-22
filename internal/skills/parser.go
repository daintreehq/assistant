package skills

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Skill file format = a tiny markdown subset:
//
//	---
//	<frontmatter>
//	---
//	<body markdown>
//
// We deliberately hand-roll the frontmatter parser (spec §2.3) instead of
// pulling in a general YAML library: the shape is owned and constrained, and a
// general parser would accept inputs the TS source rejects (and vice versa),
// breaking the validation contract.

// fenceRe matches the opening/closing --- fence, CRLF-tolerant, trailing spaces
// or tabs allowed on the fence lines. Group 1 = frontmatter; group 2 = body.
var fenceRe = regexp.MustCompile(`(?s)^---[ \t]*\r?\n(.*?)\r?\n---[ \t]*\r?\n?(.*)$`)

// kvRe matches a top-level "key: value" line. Key chars: letters, digits,
// underscore. The "\s?" consumes exactly zero or one space after the colon; the
// rest is the value.
var kvRe = regexp.MustCompile(`^([A-Za-z0-9_]+):[ \t]?(.*)$`)

// leadingBlankRe strips a run of blank lines before the opening fence.
var leadingBlankRe = regexp.MustCompile(`^(?:[ \t]*\r?\n)+`)

// listItemRe matches a block-list item line ("  - value").
var listItemRe = regexp.MustCompile(`^\s*-\s+(.*)$`)

// intRe matches a plain signed integer with no dots (so dotted versions like
// 0.2.0 stay strings — see coerceScalar).
var intRe = regexp.MustCompile(`^-?\d+$`)

// scalar is the coerced value of one frontmatter entry: string | int | bool, or
// a []scalar for arrays. We keep the discriminated value rather than mapping to
// `any` directly so validation can detect type mismatches the way Zod would.
type scalar struct {
	kind  scalarKind
	str   string
	num   int
	boolV bool
	list  []scalar
}

type scalarKind int

const (
	scalarString scalarKind = iota
	scalarInt
	scalarBool
	scalarList
)

// coerceScalar reproduces spec §2.4 exactly: trim, then in order — strip one
// layer of matching quotes (no further coercion); true/false → bool; plain
// signed integer (no dots) → int; else the trimmed string.
func coerceScalar(raw string) scalar {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return scalar{kind: scalarString, str: v[1 : len(v)-1]}
		}
	}
	switch v {
	case "true":
		return scalar{kind: scalarBool, boolV: true}
	case "false":
		return scalar{kind: scalarBool, boolV: false}
	}
	if intRe.MatchString(v) {
		// Errors here are impossible given the regex, but guard anyway.
		if n, err := strconv.Atoi(v); err == nil {
			return scalar{kind: scalarInt, num: n}
		}
	}
	return scalar{kind: scalarString, str: v}
}

// parseFrontmatter implements the hand-rolled tiny grammar (spec §2.3). It walks
// the frontmatter block line by line. Duplicate keys, indented strays, and
// malformed lines all error with the filename.
func parseFrontmatter(filename, block string) (map[string]scalar, error) {
	// Normalize CRLF so line handling is uniform.
	lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
	out := map[string]scalar{}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A line starting with whitespace that was NOT consumed as a block-list
		// item is a stray indented line — reject (a list item is only consumed
		// in the rest=="" branch below, which advances past it).
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			return nil, fmt.Errorf("%s: unexpected indented line: %q", filename, line)
		}
		m := kvRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s: malformed frontmatter line: %q", filename, line)
		}
		key, rest := m[1], m[2]
		if _, dup := out[key]; dup {
			// Prevents a second `requiredTools:` silently clobbering the first.
			return nil, fmt.Errorf("%s: duplicate frontmatter key: %q", filename, key)
		}

		switch {
		case rest == "":
			// Block list: consume following indented "- item" lines.
			items := []scalar{}
			for i+1 < len(lines) {
				lm := listItemRe.FindStringSubmatch(lines[i+1])
				if lm == nil {
					break
				}
				items = append(items, coerceScalar(lm[1]))
				i++
			}
			out[key] = scalar{kind: scalarList, list: items}
		case strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]"):
			// Inline array. Empty inner → []. Else split on ',', coerce each,
			// then drop "" entries so "[a, b,]" doesn't smuggle a blank.
			inner := strings.TrimSpace(rest[1 : len(rest)-1])
			items := []scalar{}
			if inner != "" {
				for _, part := range strings.Split(inner, ",") {
					sc := coerceScalar(part)
					if sc.kind == scalarString && sc.str == "" {
						continue
					}
					items = append(items, sc)
				}
			}
			out[key] = scalar{kind: scalarList, list: items}
		default:
			out[key] = coerceScalar(rest)
		}
	}
	return out, nil
}

// parseSkillFile parses one skill file's content (spec §2.2). filename is used
// only for error messages.
func parseSkillFile(content, filename string) (Skill, error) {
	text := content
	// Strip a leading UTF-8 BOM (U+FEFF) — written as the rune escape so the
	// source file itself doesn't carry a literal BOM.
	text = strings.TrimPrefix(text, "\uFEFF")
	// Strip a leading run of blank lines before the opening fence.
	text = leadingBlankRe.ReplaceAllString(text, "")

	m := fenceRe.FindStringSubmatch(text)
	if m == nil {
		return Skill{}, fmt.Errorf("%s: missing YAML frontmatter (a skill must open with a --- … --- block).", filename)
	}
	fmBlock, body := m[1], m[2]

	meta, err := parseFrontmatter(filename, fmBlock)
	if err != nil {
		return Skill{}, err
	}

	sk, err := buildSkill(filename, meta, strings.TrimSpace(body))
	if err != nil {
		// Mirror the TS "<filename>: invalid skill — <message>" envelope.
		return Skill{}, fmt.Errorf("%s: invalid skill — %s", filename, err.Error())
	}
	return sk, nil
}

// buildSkill assembles a Skill from coerced frontmatter + body and validates it
// the way the Zod schema would (spec §1.2). Errors carry no filename prefix —
// parseSkillFile wraps them in the "invalid skill" envelope.
func buildSkill(_ string, meta map[string]scalar, body string) (Skill, error) {
	sk := Skill{
		Tags:          []string{},
		Priority:      defaultPriority,
		MaxTurns:      defaultMaxTurns,
		Risk:          defaultRisk,
		RequiredTools: []string{},
		Body:          body,
	}

	var err error
	if sk.ID, err = requiredString(meta, "id"); err != nil {
		return Skill{}, err
	}
	if sk.Title, err = requiredString(meta, "title"); err != nil {
		return Skill{}, err
	}
	if sk.Version, err = requiredString(meta, "version"); err != nil {
		return Skill{}, err
	}
	if sk.Summary, err = requiredString(meta, "summary"); err != nil {
		return Skill{}, err
	}
	if sk.WhenToUse, err = requiredString(meta, "whenToUse"); err != nil {
		return Skill{}, err
	}

	if sc, ok := meta["tags"]; ok {
		if sk.Tags, err = stringList(sc, "tags"); err != nil {
			return Skill{}, err
		}
	}
	if sc, ok := meta["priority"]; ok {
		if sk.Priority, err = intValue(sc, "priority"); err != nil {
			return Skill{}, err
		}
	}
	if sc, ok := meta["maxTurns"]; ok {
		if sk.MaxTurns, err = intValue(sc, "maxTurns"); err != nil {
			return Skill{}, err
		}
		if sk.MaxTurns <= 0 {
			return Skill{}, fmt.Errorf("maxTurns must be a positive integer")
		}
	}
	if sc, ok := meta["risk"]; ok {
		if sc.kind != scalarString {
			return Skill{}, fmt.Errorf("risk must be a string")
		}
		r := SkillRisk(sc.str)
		if !validSkillRisk[r] {
			return Skill{}, fmt.Errorf("risk must be one of read|local|ui|terminal|project|git|external|system, got %q", sc.str)
		}
		sk.Risk = r
	}
	if sc, ok := meta["requiredTools"]; ok {
		if sk.RequiredTools, err = stringList(sc, "requiredTools"); err != nil {
			return Skill{}, err
		}
	}

	if strings.TrimSpace(sk.Body) == "" {
		return Skill{}, fmt.Errorf("body is required (the markdown after the frontmatter must be non-empty)")
	}
	return sk, nil
}

// requiredString enforces presence + a string scalar + min(1) (non-empty),
// matching z.string().min(1).
func requiredString(meta map[string]scalar, key string) (string, error) {
	sc, ok := meta[key]
	if !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	if sc.kind != scalarString {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if sc.str == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return sc.str, nil
}

// intValue enforces z.number().int(). A bare integer in frontmatter coerces to
// scalarInt; anything else is a type error.
func intValue(sc scalar, key string) (int, error) {
	if sc.kind != scalarInt {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return sc.num, nil
}

// stringList enforces a string[] — every element must be a string scalar.
func stringList(sc scalar, key string) ([]string, error) {
	if sc.kind != scalarList {
		return nil, fmt.Errorf("%s must be a list", key)
	}
	out := make([]string, 0, len(sc.list))
	for _, item := range sc.list {
		if item.kind != scalarString {
			return nil, fmt.Errorf("%s entries must be strings", key)
		}
		out = append(out, item.str)
	}
	return out, nil
}
