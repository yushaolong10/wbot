package domain

import (
	"encoding/json"
	"time"
)

type Workspace struct {
	ID, Name, Root, Kind string
	CreatedAt            time.Time
}
type Session struct {
	ID, WorkspaceID, Title string
	CreatedAt, UpdatedAt   time.Time
}
type Message struct {
	ID, SessionID, TaskID, Role, Content  string
	Seq                                   int64
	ContentJSON                           json.RawMessage
	TokenCount                            int
	ContentHash, CompactionState          string
	ParentMessageID, ToolCallID, ToolName string
	ArtifactIDs                           []string
	Importance                            float64
	CreatedAt                             time.Time
}

type HistorySummary struct {
	Objectives       []string `json:"objectives"`
	UserConstraints  []string `json:"user_constraints"`
	VerifiedFacts    []string `json:"verified_facts"`
	Decisions        []string `json:"decisions"`
	CompletedActions []string `json:"completed_actions"`
	PendingActions   []string `json:"pending_actions"`
	FailedActions    []string `json:"failed_actions"`
	ActiveToolCalls  []string `json:"active_tool_calls"`
	Artifacts        []string `json:"artifacts"`
	MemoryIDs        []string `json:"memory_ids"`
	FileChanges      []string `json:"file_changes"`
	OpenQuestions    []string `json:"open_questions"`
}

type HistorySegment struct {
	ID, SessionID, TaskID, SummaryJSON, Model, PromptVersion, SourceHash, Status string
	Level, SourceMessageCount, TokenCount                                        int
	FirstSeq, LastSeq                                                            int64
	SourceMessageIDs, SourceSegmentIDs                                           []string
	CreatedAt                                                                    time.Time
}
type Task struct {
	ID, SessionID, Objective, Status, Result, Error string
	CreatedAt, UpdatedAt                            time.Time
}

// ---- Node & Task Graph (P1: NodeKind) ----

type NodeKind string

const (
	NodeResearch NodeKind = "research"
	NodePlan     NodeKind = "plan"
	NodeExecute  NodeKind = "execute"
	NodeVerify   NodeKind = "verify"
	NodeApproval NodeKind = "approval"
	NodeWait     NodeKind = "wait"
	NodeLoadHist NodeKind = "load_history"
	NodeRetrieve NodeKind = "retrieve_memory"
)

type ArtifactRef struct {
	ArtifactID string `json:"artifact_id"`
	Label      string `json:"label,omitempty"`
}

type OutputContract struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Node struct {
	ID, TaskID, Title, Description, Status string
	Kind                                   NodeKind         `json:"kind"`
	DependsOn                              []string         `json:"depends_on"`
	Inputs                                 []ArtifactRef    `json:"inputs,omitempty"`
	Outputs                                []OutputContract `json:"outputs,omitempty"`
	CriteriaIDs                            []string         `json:"criteria_ids,omitempty"`
	RiskLevel                              string           `json:"risk_level"`
	Attempt                                int              `json:"attempt"`
	MaxAttempts                            int              `json:"max_attempts"`
	Timeout                                time.Duration    `json:"timeout,omitempty"`
	AssignedRole                           string           `json:"assigned_role,omitempty"`
	Result                                 string           `json:"result"`
	CreatedAt, UpdatedAt                   time.Time
	StartedAt                              *time.Time
	QueueDurationMS, DurationMS            int64
}

// ---- Acceptance Criteria & Evidence (P0) ----

type CriterionType string

const (
	CriterionFileExists   CriterionType = "file_exists"
	CriterionFileContains CriterionType = "file_contains"
	CriterionCommand      CriterionType = "command"
	CriterionJSONSchema   CriterionType = "json_schema"
	CriterionArtifact     CriterionType = "artifact"
	CriterionToolResult   CriterionType = "tool_result"
	CriterionHTTPResponse CriterionType = "http_response"
	CriterionModelRubric  CriterionType = "model_rubric"
	CriterionUserApproval CriterionType = "user_approval"
)

type AcceptanceCriterion struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	NodeID      string          `json:"node_id"`
	Type        CriterionType   `json:"type"`
	Description string          `json:"description"`
	Required    bool            `json:"required"`
	Config      json.RawMessage `json:"config"`
	Status      string          `json:"status"` // pending | passed | failed
	EvidenceIDs []string        `json:"evidence_ids"`
	Reason      string          `json:"reason"`
}

type Evidence struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	NodeID      string    `json:"node_id"`
	CriterionID string    `json:"criterion_id"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`
	ArtifactID  string    `json:"artifact_id,omitempty"`
	Digest      string    `json:"digest"`
	Summary     string    `json:"summary"`
	Passed      bool      `json:"passed"`
	CollectedAt time.Time `json:"collected_at"`
}

type ArtifactInfo struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// ---- Graph Proposal & Revision (P2) ----

type NodeProposal struct {
	TempID      string                `json:"temp_id"`
	Kind        NodeKind              `json:"kind"`
	Title       string                `json:"title"`
	Description string                `json:"description,omitempty"`
	DependsOn   []string              `json:"depends_on"`
	Outputs     []OutputContract      `json:"outputs,omitempty"`
	Criteria    []AcceptanceCriterion `json:"criteria,omitempty"`
	RiskLevel   string                `json:"risk_level,omitempty"`
}

type GraphProposal struct {
	GoalSummary   string         `json:"goal_summary"`
	Assumptions   []string       `json:"assumptions,omitempty"`
	OpenQuestions []string       `json:"open_questions,omitempty"`
	Nodes         []NodeProposal `json:"nodes"`
}

type GraphRevision struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	Version      int       `json:"version"`
	Reason       string    `json:"reason"`
	PlannerModel string    `json:"planner_model"`
	SourceEvent  string    `json:"source_event,omitempty"`
	NodesJSON    string    `json:"nodes_json"`
	CreatedAt    time.Time `json:"created_at"`
}

// ---- Completion Gate (P0) ----

type GateAction string

const (
	ActionComplete        GateAction = "complete"
	ActionRetry           GateAction = "retry"
	ActionReplan          GateAction = "replan"
	ActionWaitForUser     GateAction = "wait_for_user"
	ActionWaitForExternal GateAction = "wait_for_external"
	ActionFail            GateAction = "fail"
)

type GateResult struct {
	Action      GateAction `json:"action"`
	Reason      string     `json:"reason"`
	PassedCount int        `json:"passed_count"`
	FailedCount int        `json:"failed_count"`
	TotalCount  int        `json:"total_count"`
}

// ---- Legacy compatibility ----

// AcceptanceCriterionLegacy is kept for reading old table format during migration.
type AcceptanceCriterionLegacy struct {
	TaskID    string `json:"task_id"`
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason"`
}

type TaskTiming struct {
	ModelCalls, ModelDurationMS int64
}
type Approval struct {
	ID, TaskID, NodeID, ToolName, Arguments, ArgumentsDigest, RiskLevel, Reason, Status string
	CreatedAt                                                                           time.Time
}
type Event struct {
	ID                      int64 `json:"id"`
	SessionID, TaskID, Type string
	Payload                 any
	CreatedAt               time.Time
}
type ToolCall struct {
	ID, Name  string
	Arguments map[string]any
}
type ToolResult struct {
	ToolCallID, Status string
	Data               any
	Summary            string
	Artifacts          []string
	Retryable          bool
	Error              *ToolError
	DurationMS         int64
}
type ToolError struct {
	Code, Message string
	Retryable     bool
	Details       any
}
