package storage

import "github.com/daintreehq/daintree-assistant/internal/ports"

// Compile-time assertion that *Store satisfies the ports.Store seam the agent
// loop / daemon depend on (AppendRunEvent, AppendAudit, Close).
var _ ports.Store = (*Store)(nil)
