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
	CreatedAt              time.Time
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
type AcceptanceCriterion struct {
	TaskID, Criterion string
	Passed            bool
	Reason            string
}
type Node struct {
	ID, TaskID, Title, Description, Status string
	DependsOn                              []string
	RiskLevel                              string
	Attempt, MaxAttempts                   int
	Result                                 string
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
