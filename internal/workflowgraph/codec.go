package workflowgraph

import (
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// EncodeSnapshot serializes a graph into its durable WorkflowGraphRecord
// (promoted columns + the opaque snapshot JSON). Revision is NOT set here —
// the storage layer owns it (1 on insert, +1 per successful update).
func EncodeSnapshot(g *Graph) (domain.WorkflowGraphRecord, error) {
	if g.SchemaVersion == 0 {
		g.SchemaVersion = SnapshotSchemaVersion
	}
	b, err := json.Marshal(g)
	if err != nil {
		return domain.WorkflowGraphRecord{}, fmt.Errorf("encode graph snapshot: %w", err)
	}
	return domain.WorkflowGraphRecord{
		ID:            g.ID,
		Status:        string(g.Status),
		Goal:          g.Goal,
		SchemaVersion: g.SchemaVersion,
		SnapshotJson:  string(b),
		CreatedAt:     g.CreatedAt,
		UpdatedAt:     g.UpdatedAt,
		CompletedAt:   g.CompletedAt,
	}, nil
}

// DecodeSnapshot deserializes a stored record back into the typed graph. A
// snapshot from a NEWER writer than this build understands is refused rather
// than guessed at — the caller surfaces it as unreadable, never silently
// half-parses it.
func DecodeSnapshot(rec *domain.WorkflowGraphRecord) (*Graph, error) {
	if rec == nil {
		return nil, fmt.Errorf("nil workflow graph record")
	}
	var g Graph
	if err := json.Unmarshal([]byte(rec.SnapshotJson), &g); err != nil {
		return nil, fmt.Errorf("decode graph snapshot %s: %w", rec.ID, err)
	}
	if g.SchemaVersion > SnapshotSchemaVersion {
		return nil, fmt.Errorf("graph snapshot %s has schema version %d (this build reads ≤ %d)",
			rec.ID, g.SchemaVersion, SnapshotSchemaVersion)
	}
	// The promoted columns are authoritative for identity; the snapshot for
	// structure. A drifted id would corrupt every downstream lookup.
	if g.ID != rec.ID {
		return nil, fmt.Errorf("graph snapshot id %q does not match record id %q", g.ID, rec.ID)
	}
	return &g, nil
}
