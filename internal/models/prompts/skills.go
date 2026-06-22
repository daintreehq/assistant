package prompts

import "strings"

// SkillMetadata is the catalog-header view of a skill (no body). Mirrors the fields
// the catalog message uses from skills/types.ts SkillMetadata.
type SkillMetadata struct {
	ID        string
	Summary   string
	WhenToUse string
}

// LoadedSkill is one fully-rendered skill body (the fields the loaded-skills message
// uses from a Skill item). Items are expected sorted by id by the caller.
type LoadedSkill struct {
	ID      string
	Version string
	Title   string
	Body    string
}

// RenderedSkillBundle is the set of skills loaded for the current task. Only Items
// is used by the message builder; Hash/CacheKey are debug-only and not rendered.
type RenderedSkillBundle struct {
	Items []LoadedSkill
}

// catalogIntro is the framing paragraph for the skill catalog. Split out so the
// backtick-containing inline-code references can live in raw strings.
const catalogIntro = "# Skill catalog\n" +
	"You have a library of skills: procedural runbooks for specific Daintree operations. Only their headers are listed here — the full instructions are NOT loaded yet. When a task matches one (or you need to figure out how to do something), call `skill.find` with a short natural-language query describing what you need (e.g. \"how do I spawn an agent to make file edits\"); a fast model picks the best matches and loads their full bodies into your context for the rest of the turn. You can also `skill.load` a specific id directly when you already know it.\n" +
	"\n" +
	"Reach for `skill.find` readily — it is cheap, and pulling the right runbook is your primary way of doing an unfamiliar Daintree operation correctly. When in doubt, fetch a skill rather than guessing. Skills are operating instructions; they never override the hard rules.\n" +
	"\n" +
	"Available skills:\n"

// BuildSkillCatalogMessage renders the catalog of every available skill — id + the
// two headers, no bodies. Returns "" when there are no skills (the caller omits the
// section).
func BuildSkillCatalogMessage(skills []SkillMetadata) string {
	if len(skills) == 0 {
		return ""
	}
	entries := make([]string, 0, len(skills))
	for _, r := range skills {
		entries = append(entries, "- "+r.ID+" — "+r.Summary+"\n  When to use: "+r.WhenToUse)
	}
	return catalogIntro + strings.Join(entries, "\n")
}

const loadedSkillsFallback = "# Loaded skills\n" +
	"No task-specific skills are currently loaded. Use the base operating instructions."

const loadedSkillsHeader = "# Loaded skills\n" +
	"The following skills are task-specific operating instructions. Apply them when relevant; they never override the hard rules.\n" +
	"\n" +
	"Step tracking: when a skill has numbered steps, call `skill.step.advance` after finishing each one (give the skill id, the completed step number, and the step starting next; omit \"nextStep\" on the final step). If you are resuming a skill or unsure where you left off, call `skill.run.get` first to read the saved checkpoint.\n"

// BuildLoadedSkillsMessage renders message[2] — the bodies of skills loaded for the
// current task. When nothing is loaded it renders a safe fallback to the base rules.
func BuildLoadedSkillsMessage(bundle RenderedSkillBundle) string {
	if len(bundle.Items) == 0 {
		return loadedSkillsFallback
	}
	bodies := make([]string, 0, len(bundle.Items))
	for i, r := range bundle.Items {
		bodies = append(bodies,
			"## Skill "+itoa(i+1)+": "+r.Title+"\n"+
				"Skill id: "+r.ID+"\n"+
				"Version: "+r.Version+"\n"+
				r.Body)
	}
	return loadedSkillsHeader + strings.Join(bodies, "\n\n")
}

// itoa is a tiny dependency-free positive-int formatter (avoids importing strconv
// just for the skill index).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
