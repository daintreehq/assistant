package domain

import "errors"

// ErrWorkflowGraphRevisionConflict reports that an optimistic workflow-graph
// snapshot write named a revision the store had already moved past (a
// concurrent writer won). Lives in domain (not storage) so the workflowgraph
// service can detect it with errors.Is without importing the concrete store.
var ErrWorkflowGraphRevisionConflict = errors.New("workflow graph revision conflict")
