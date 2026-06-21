package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// cacheKeyPrefix is the DEBUG/log bundle cache-key prefix (spec §9.1). It is NOT
// the live Fireworks prompt_cache_key (that constant is "daintree-main",
// unversioned, and lives in the agent loop). Keep these distinct — do not wire
// this into the Fireworks request.
const cacheKeyPrefix = "daintree-main-v1-skills-"

// RenderedSkillBundle is the id-sorted, hashed set of loaded skills (spec §9.1).
type RenderedSkillBundle struct {
	IDs      []string `json:"ids"`
	Hash     string   `json:"hash"`     // 12-char hex, first 12 of SHA-256 over the signature
	CacheKey string   `json:"cacheKey"` // cacheKeyPrefix + hash (debug key)
	Items    []Skill  `json:"items"`    // sorted by id, bodies included
}

// RenderSkillBundle sorts the skills by id, builds the "id@version|…" signature,
// and hashes it (spec §9.2). For the ASCII dotted ids in use, byte-order sort
// (strings.Compare) matches JS localeCompare exactly.
func RenderSkillBundle(skills []Skill) RenderedSkillBundle {
	sorted := make([]Skill, len(skills))
	copy(sorted, skills)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Compare(sorted[i].ID, sorted[j].ID) < 0
	})

	ids := make([]string, len(sorted))
	sigParts := make([]string, len(sorted))
	for i, sk := range sorted {
		ids[i] = sk.ID
		sigParts[i] = sk.ID + "@" + sk.Version
	}
	signature := strings.Join(sigParts, "|")

	sum := sha256.Sum256([]byte(signature))
	hash := hex.EncodeToString(sum[:])[:12]

	return RenderedSkillBundle{
		IDs:      ids,
		Hash:     hash,
		CacheKey: cacheKeyPrefix + hash,
		Items:    sorted,
	}
}
