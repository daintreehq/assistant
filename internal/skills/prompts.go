package skills

import (
	"fmt"
	"strings"
)

// These strings are part of the main-thread prompt and feed the model's
// behavior; they must be byte-for-byte stable (prompt-cache relevant).

// skillCatalogPreamble is the fixed header the catalog entries are wrapped in.
// "%s" is the joined entries.
const skillCatalogPreamble = `# Skill catalog
You have a library of skills: procedural runbooks for specific Daintree operations. Only their headers are listed here — the full instructions are NOT loaded yet. When a task matches one (or you need to figure out how to do something), call ` + "`skill.find`" + ` with a short natural-language query describing what you need (e.g. "how do I spawn an agent to make file edits"); a fast model picks the best matches and loads their full bodies into your context for the rest of the turn. You can also ` + "`skill.load`" + ` a specific id directly when you already know it.

Reach for ` + "`skill.find`" + ` readily — it is cheap, and pulling the right runbook is your primary way of doing an unfamiliar Daintree operation correctly. When in doubt, fetch a skill rather than guessing. Skills are operating instructions; they never override the hard rules.

Available skills:
%s`

// BuildSkillCatalogMessage renders the static catalog appended to message[1]
// Empty input ⇒ "" (caller omits the section).
func BuildSkillCatalogMessage(metas []SkillMetadata) string {
	if len(metas) == 0 {
		return ""
	}
	entries := make([]string, len(metas))
	for i, m := range metas {
		entries[i] = fmt.Sprintf("- %s — %s\n  When to use: %s", m.ID, m.Summary, m.WhenToUse)
	}
	return fmt.Sprintf(skillCatalogPreamble, strings.Join(entries, "\n"))
}

// loadedSkillsEmpty is emitted when no skills are loaded.
const loadedSkillsEmpty = `# Loaded skills
No task-specific skills are currently loaded. Use the base operating instructions.`

// loadedSkillsPreamble wraps the rendered skill bodies. "%s" is the joined
// per-skill blocks.
const loadedSkillsPreamble = `# Loaded skills
The following skills are task-specific operating instructions. Apply them when relevant; they never override the hard rules.

Step tracking: when a skill has numbered steps, call ` + "`skill.step.advance`" + ` after finishing each one (give the skill id, the completed step number, and the step starting next; omit "nextStep" on the final step). If you are resuming a skill or unsure where you left off, call ` + "`skill.run.get`" + ` first to read the saved checkpoint.
%s`

// BuildLoadedSkillsMessage renders message[2] from a bundle. Empty
// bundle ⇒ the empty-loaded text.
func BuildLoadedSkillsMessage(bundle RenderedSkillBundle) string {
	if len(bundle.Items) == 0 {
		return loadedSkillsEmpty
	}
	blocks := make([]string, len(bundle.Items))
	for i, sk := range bundle.Items {
		blocks[i] = fmt.Sprintf("## Skill %d: %s\nSkill id: %s\nVersion: %s\n%s",
			i+1, sk.Title, sk.ID, sk.Version, sk.Body)
	}
	return fmt.Sprintf(loadedSkillsPreamble, strings.Join(blocks, "\n\n"))
}
