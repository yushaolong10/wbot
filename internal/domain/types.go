package domain

import "time"

type Workspace struct {
	ID, Name, Root, Kind string
	CreatedAt            time.Time
}
type Session struct {
	ID, WorkspaceID, Title string
	CreatedAt              time.Time
}
type Message struct {
	ID, SessionID, TaskID, Role, Content string
	CreatedAt                            time.Time
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
