package domain

import (
	"strings"

	"github.com/google/uuid"
)

// ID prefixes. An ID is "<prefix><first 8 hex chars of a v4 UUID>".
// The TS version slices a hyphenated UUIDv4 string [:8] — the first 8 hex chars
// of the time-low field. The bytes are random, so the exact value need not match
// the TS output; only the "<prefix>_" + 8 lowercase hex shape is load-bearing.
const (
	PrefixTimer       = "tmr_"
	PrefixWatcher     = "wch_"
	PrefixEvent       = "evt_"
	PrefixAudit       = "aud_"
	PrefixRunEvent    = "rne_"
	PrefixGrant       = "grt_"
	PrefixMessage     = "msg_"
	PrefixSkillSel    = "rsl_"
	PrefixWorkflow    = "wfr_"
	PrefixSkillRun    = "rrs_"
	PrefixMemory      = "mem_"
	PrefixAgentLaunch = "agt_"
)

// NewID returns prefix followed by the first 8 hex chars of a fresh v4 UUID.
func NewID(prefix string) string {
	hex := strings.ReplaceAll(uuid.NewString(), "-", "")
	return prefix + hex[:8]
}
