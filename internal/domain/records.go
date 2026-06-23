package domain

// Persisted DB-row records. All *At fields are epoch-ms
// int64. Optionals are pointers. `*Json` fields hold a JSON-serialized string of
// the named shape; the storage layer (de)serializes them.

// TimerRecord is a scheduled timer (persists across sessions).
type TimerRecord struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	FireAt        int64   `json:"fireAt"`
	RepeatEveryMs *int64  `json:"repeatEveryMs,omitempty"`
	RepeatUntil   *int64  `json:"repeatUntil,omitempty"`
	MaxRuns       *int    `json:"maxRuns,omitempty"`
	RunCount      int     `json:"runCount"`
	PayloadType   string  `json:"payloadType"` // enqueue | run_check | call_safe_tool
	PayloadJson   string  `json:"payloadJson"`
	TargetJson    *string `json:"targetJson,omitempty"`
	Status        string  `json:"status"` // scheduled | fired | cancelled | done
	CreatedAt     int64   `json:"createdAt"`
	LastFiredAt   *int64  `json:"lastFiredAt,omitempty"`
}

// WatcherRecord supervises a terminal or PR. Session-scoped (cancelled on DB
// open if non-terminal). Unknown kind fails closed to "error".
type WatcherRecord struct {
	ID                 string         `json:"id"`
	Kind               string         `json:"kind"` // terminal | pr_state
	Title              string         `json:"title"`
	Goal               string         `json:"goal"`
	TargetsJson        string         `json:"targetsJson"` // JSON string[]
	CadenceMs          int            `json:"cadenceMs"`
	IsSupervisor       *bool          `json:"isSupervisor,omitempty"`
	ModelTier          ModelTier      `json:"modelTier"`
	StartAfterMs       *int64         `json:"startAfterMs,omitempty"`
	StopAfterMs        *int64         `json:"stopAfterMs,omitempty"`
	StopWhenJson       *string        `json:"stopWhenJson,omitempty"`
	AlertWhenJson      *string        `json:"alertWhenJson,omitempty"`
	OptionsJson        *string        `json:"optionsJson,omitempty"`
	Status             string         `json:"status"` // created|active|paused|condition_met|timeout|cancelled|error
	LastClassification *string        `json:"lastClassification,omitempty"`
	LastEpistemicKind  *EpistemicKind `json:"lastEpistemicKind,omitempty"`
	LastCheckedAt      *int64         `json:"lastCheckedAt,omitempty"`
	NextCheckAt        int64          `json:"nextCheckAt"` // required
	CreatedAt          int64          `json:"createdAt"`
	// EndedReason distinguishes a session-boundary teardown ("session_ended", set by
	// cancelStaleWatchers on the next open) from a deliberate user cancel
	// ("user_cancelled", set by watcher.cancel). nil on active rows and on natural
	// terminal states (condition_met/timeout/error). EndedAt is when that cancel
	// happened.
	EndedReason *string `json:"endedReason,omitempty"`
	EndedAt     *int64  `json:"endedAt,omitempty"`
}

// AuditRecord is one row of the audit log.
type AuditRecord struct {
	ID          string                 `json:"id"`
	Ts          int64                  `json:"ts"`
	Actor       ToolActor              `json:"actor"`
	ToolName    string                 `json:"toolName"`
	ArgsJson    string                 `json:"argsJson"`
	Outcome     string                 `json:"outcome"` // ok|error|denied|dedup|grant_ok
	DurationMs  int64                  `json:"durationMs"`
	Summary     string                 `json:"summary"`
	ResultJson  *string                `json:"resultJson,omitempty"`
	GrantSource *AutomationGrantSource `json:"grantSource,omitempty"`
	GrantID     *string                `json:"grantId,omitempty"`
	RunID       *string                `json:"runId,omitempty"`
}

// RunEventRecord is a durable event in a turn's replay log.
type RunEventRecord struct {
	ID      string  `json:"id"` // rne_<uuid8>
	RunID   string  `json:"runId"`
	Seq     int     `json:"seq"` // from 0
	Ts      int64   `json:"ts"`
	Type    string  `json:"type"` // e.g. "assistant:start"
	Payload *string `json:"payload,omitempty"`
}

// RunSummaryRecord is computed (not persisted) over a run's events.
type RunSummaryRecord struct {
	RunID      string `json:"runId"`
	FirstTs    int64  `json:"firstTs"`
	LastTs     int64  `json:"lastTs"`
	EventCount int    `json:"eventCount"`
}

// AutomationGrantRecord authorizes a non-interactive actor to run mutating tools.
// Authorization = tool-name in allowedToolNames OR risk in allowedRiskClasses
// (union); at least one list must be non-empty (enforced in code). revokedAt is
// an explicit revoke ONLY — use-exhaustion is usesRemaining==0 and does NOT
// stamp revokedAt.
type AutomationGrantRecord struct {
	ID                     string                   `json:"id"`
	ActorID                string                   `json:"actorId"` // wch_… | tmr_…
	ActorType              AutomationGrantActorType `json:"actorType"`
	AllowedRiskClassesJson *string                  `json:"allowedRiskClassesJson"` // string | null
	AllowedToolNamesJson   *string                  `json:"allowedToolNamesJson"`   // string | null
	ExpiresAt              int64                    `json:"expiresAt"`
	MaxUses                int                      `json:"maxUses"`
	UsesRemaining          int                      `json:"usesRemaining"`
	RevokedAt              *int64                   `json:"revokedAt"` // null until explicit revoke
	CreatedAt              int64                    `json:"createdAt"`
	Source                 AutomationGrantSource    `json:"source"`
}

// ConversationMessageRecord is one persisted conversation message.
type ConversationMessageRecord struct {
	ID            string  `json:"id"` // msg_<uuid8>
	SessionID     string  `json:"sessionId"`
	Seq           int     `json:"seq"`
	Role          string  `json:"role"` // system|user|assistant|tool
	Content       string  `json:"content"`
	ToolCallsJson *string `json:"toolCallsJson,omitempty"`
	ToolCallID    *string `json:"toolCallId,omitempty"`
	CreatedAt     int64   `json:"createdAt"`
}

// SkillSelectionLogRecord records a skill-selection decision.
type SkillSelectionLogRecord struct {
	ID                   string  `json:"id"` // rsl_<uuid8>
	Ts                   int64   `json:"ts"`
	SessionID            string  `json:"sessionId"`
	UserInput            string  `json:"userInput"`
	SelectedSkillIdsJson string  `json:"selectedSkillIdsJson"`
	Confidence           float64 `json:"confidence"`
	TaskType             *string `json:"taskType,omitempty"`
	Reason               *string `json:"reason,omitempty"`
}

// WorkflowRunRecord tracks an end-to-end workflow run.
type WorkflowRunRecord struct {
	ID                string            `json:"id"` // wfr_<uuid8>
	IssueNumber       *int              `json:"issueNumber,omitempty"`
	IssueURL          *string           `json:"issueUrl,omitempty"`
	IssueTitle        *string           `json:"issueTitle,omitempty"`
	Branch            *string           `json:"branch,omitempty"`
	WorktreeID        *string           `json:"worktreeId,omitempty"`
	PRNumber          *int              `json:"prNumber,omitempty"`
	PRURL             *string           `json:"prUrl,omitempty"`
	TerminalIdsJson   *string           `json:"terminalIdsJson,omitempty"`
	WatcherIdsJson    *string           `json:"watcherIdsJson,omitempty"`
	QueueEventIdsJson *string           `json:"queueEventIdsJson,omitempty"`
	Status            WorkflowRunStatus `json:"status"`
	NextActionJson    *string           `json:"nextActionJson,omitempty"` // serialized RecommendedAction
	NotesJson         *string           `json:"notesJson,omitempty"`      // JSON string[]
	CreatedAt         int64             `json:"createdAt"`
	UpdatedAt         int64             `json:"updatedAt"` // required
	CompletedAt       *int64            `json:"completedAt,omitempty"`
}

// MemoryRecord is one stored memory. forget is a soft-delete (stamp DeletedAt);
// recall/list filter DeletedAt IS NULL. One DB == one project (no projectId).
type MemoryRecord struct {
	ID        string       `json:"id"` // mem_<uuid8>
	Content   string       `json:"content"`
	Category  *string      `json:"category,omitempty"`
	Source    MemorySource `json:"source"`
	PinnedAt  *int64       `json:"pinnedAt,omitempty"`
	DeletedAt *int64       `json:"deletedAt,omitempty"`
	CreatedAt int64        `json:"createdAt"`
	UpdatedAt int64        `json:"updatedAt"`
}

// SkillStepProgress is one step within a SkillRunStateRecord (1-based index).
type SkillStepProgress struct {
	Index  int             `json:"index"`
	Status SkillStepStatus `json:"status"`
	Notes  *string         `json:"notes,omitempty"`
	Ts     int64           `json:"ts"`
}

// SkillRunStateRecord tracks a skill's stepwise progress. Natural key
// (sessionId, skillId).
type SkillRunStateRecord struct {
	ID          string         `json:"id"` // rrs_<uuid8>
	SessionID   string         `json:"sessionId"`
	SkillID     string         `json:"skillId"`
	CurrentStep int            `json:"currentStep"` // 0 = not started
	StepsJson   string         `json:"stepsJson"`   // JSON SkillStepProgress[]
	Status      SkillRunStatus `json:"status"`
	StartedAt   int64          `json:"startedAt"`
	UpdatedAt   int64          `json:"updatedAt"`
	CompletedAt *int64         `json:"completedAt,omitempty"`
}

// AgentLaunchRecord is the durable state of the idempotent agent-spawn saga.
// Session-scoped: cancelStaleAgentLaunches marks non-terminal rows failed on DB
// open.
type AgentLaunchRecord struct {
	ID             string           `json:"id"` // agt_<uuid8>
	IdempotencyKey string           `json:"idempotencyKey"`
	AgentID        string           `json:"agentId"`
	WorktreeID     *string          `json:"worktreeId,omitempty"`
	Mode           string           `json:"mode"` // edit | explore
	Title          string           `json:"title"`
	Name           string           `json:"name"`
	TerminalID     *string          `json:"terminalId,omitempty"`
	WatcherID      *string          `json:"watcherId,omitempty"`
	Stage          AgentLaunchStage `json:"stage"`
	ErrorCode      *string          `json:"errorCode,omitempty"`
	ErrorMessage   *string          `json:"errorMessage,omitempty"`
	CreatedAt      int64            `json:"createdAt"`
	UpdatedAt      int64            `json:"updatedAt"`
}

// QueueEvent is a live attention-queue event. updatedAt advances
// on each dedupe bump; createdAt stays fixed (recency).
type QueueEvent struct {
	ID                 string              `json:"id"`
	Source             EventSource         `json:"source"`
	Severity           Severity            `json:"severity"`
	Title              string              `json:"title"`
	Summary            string              `json:"summary"`
	Target             *EventTarget        `json:"target,omitempty"`
	Evidence           []string            `json:"evidence,omitempty"`
	RecommendedActions []RecommendedAction `json:"recommendedActions,omitempty"`
	DedupeKey          string              `json:"dedupeKey,omitempty"`
	EpistemicKind      EpistemicKind       `json:"epistemicKind,omitempty"`
	CreatedAt          int64               `json:"createdAt"`
	UpdatedAt          *int64              `json:"updatedAt,omitempty"`
	ExpiresAt          *int64              `json:"expiresAt,omitempty"`
	ResolvedAt         *int64              `json:"resolvedAt,omitempty"`
	Count              int                 `json:"count"`
}

// QueueDigestOptions filters a queue digest.
type QueueDigestOptions struct {
	SeverityAtLeast *Severity
	MaxItems        *int
	IncludeResolved bool
	NotifiedIsNull  bool
}
