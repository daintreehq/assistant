package skills

import (
	"encoding/json"
	"fmt"
	"strings"
)

// marshalStringSlice JSON-encodes a string slice, always producing "[]" for an
// empty/nil slice (never "null") so the persisted selectedSkillIdsJson is a
// valid JSON array.
func marshalStringSlice(ss []string) (string, error) {
	if ss == nil {
		ss = []string{}
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Describe renders the "/skills loaded" text. Empty bundle ⇒ the
// no-skills line; else a header with count + bundle hash and one line per item.
func (a *ActiveSkills) Describe() string {
	if len(a.bundle.Items) == 0 {
		return "No skills are currently loaded."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Loaded skills (%d, bundle %s):", len(a.bundle.Items), a.bundle.Hash)
	for _, sk := range a.bundle.Items {
		fmt.Fprintf(&b, "\n  %s  [%s]  %s — %s", sk.ID, sk.Risk, sk.Title, sk.Summary)
	}
	return b.String()
}

// LoadedSkillsMessage renders the messages[2] content from the current bundle.
// Convenience for the agent loop (the only message it should rewrite on a skill
// change).
func (a *ActiveSkills) LoadedSkillsMessage() string {
	return BuildLoadedSkillsMessage(a.bundle)
}
