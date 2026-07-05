package domain

import (
	"strings"

	"github.com/google/uuid"
)

// ID prefixes. An ID is "<prefix><first 8 hex chars of a v4 UUID>".
// The bytes are random; only the "<prefix>_" + 8 lowercase hex shape is
// load-bearing.
const (
	PrefixTimer       = "tmr_"
	PrefixWatcher     = "wch_"
	PrefixEvent       = "evt_"
	PrefixAudit       = "aud_"
	PrefixRunEvent    = "rne_"
	PrefixGrant       = "grt_"
	PrefixMessage     = "msg_"
	PrefixWorkflow    = "wfr_"
	PrefixSkillRun    = "rrs_"
	PrefixMemory      = "mem_"
	PrefixAgentLaunch = "agt_"
	PrefixScratch     = "scr_"
	PrefixAsync       = "asy_"
	// PrefixArtifact tags an oversized tool-result overflow payload. Longer than the
	// others for historical continuity — the truncation stub + artifact.read have
	// always surfaced this id as "artifact_<8hex>", so the literal shape is wire.
	PrefixArtifact = "artifact_"
)

// NewID returns prefix followed by the first 8 hex chars of a fresh v4 UUID.
func NewID(prefix string) string {
	hex := strings.ReplaceAll(uuid.NewString(), "-", "")
	return prefix + hex[:8]
}
