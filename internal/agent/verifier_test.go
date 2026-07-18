package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
)

func TestVerifierFileExists(t *testing.T) {
	v := &FileExistsVerifier{}
	if v.Type() != domain.CriterionFileExists {
		t.Fatal("wrong type")
	}

	// Test with existing file
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0644)

	cfg, _ := json.Marshal(map[string]string{"path": path})
	c := domain.AcceptanceCriterion{Type: domain.CriterionFileExists, Config: cfg, ID: "c1", TaskID: "t1"}

	ev, err := v.Verify(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Passed {
		t.Fatal("expected file_exists to pass")
	}

	// Test with missing file
	cfg2, _ := json.Marshal(map[string]string{"path": filepath.Join(tmpDir, "nope.txt")})
	c2 := domain.AcceptanceCriterion{Type: domain.CriterionFileExists, Config: cfg2, ID: "c2", TaskID: "t1"}
	ev2, err := v.Verify(context.Background(), c2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Passed {
		t.Fatal("expected file_exists to fail for missing file")
	}
}

func TestVerifierCommand(t *testing.T) {
	store, policy, taskID, root := newVerifierTask(t, "full_access")
	v := &CommandVerifier{permissions: policy}
	if v.Type() != domain.CriterionCommand {
		t.Fatal("wrong type")
	}

	cfg, _ := json.Marshal(map[string]any{"command": "test \"$PWD\" = " + shellQuote(root), "expected_exit_code": 0})
	c := domain.AcceptanceCriterion{Type: domain.CriterionCommand, Config: cfg, ID: "c1", TaskID: taskID}

	ev, err := v.Verify(context.Background(), c, store)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Passed {
		t.Fatal("expected command to pass with exit 0")
	}

	// Test failing command
	cfg2, _ := json.Marshal(map[string]any{"command": "exit 1", "expected_exit_code": 0})
	c2 := domain.AcceptanceCriterion{Type: domain.CriterionCommand, Config: cfg2, ID: "c2", TaskID: taskID}
	ev2, err := v.Verify(context.Background(), c2, store)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Passed {
		t.Fatal("expected command to fail with non-zero exit")
	}
}

func TestVerifierRejectsWorkspaceEscapeAndUnapprovedCommand(t *testing.T) {
	store, policy, taskID, _ := newVerifierTask(t, "approval")
	registry := NewVerifierRegistry(policy)
	pathConfig, _ := json.Marshal(map[string]string{"path": "../outside.txt"})
	if _, err := registry.Verify(context.Background(), domain.AcceptanceCriterion{ID: "path", TaskID: taskID, Type: domain.CriterionFileExists, Config: pathConfig}, store); err == nil {
		t.Fatal("expected workspace escape to be rejected")
	}
	commandConfig, _ := json.Marshal(map[string]any{"command": "echo unsafe", "expected_exit_code": 0})
	if _, err := registry.Verify(context.Background(), domain.AcceptanceCriterion{ID: "cmd", TaskID: taskID, Type: domain.CriterionCommand, Config: commandConfig}, store); err == nil {
		t.Fatal("expected unapproved verification command to be rejected")
	}
}

func TestToolResultVerifierDoesNotHideEarlierError(t *testing.T) {
	store, _, taskID, _ := newVerifierTask(t, "full_access")
	task, _ := store.Task(context.Background(), taskID)
	calls := []domain.ToolCall{
		{ID: "failed-call", Name: "filesystem.write", Arguments: map[string]any{"path": "a.txt", "content": "x"}},
		{ID: "success-call", Name: "filesystem.read", Arguments: map[string]any{"path": "b.txt"}},
	}
	rawCalls, _ := json.Marshal(calls)
	_, _ = store.AddStructuredMessage(context.Background(), domain.Message{SessionID: task.SessionID, TaskID: taskID, Role: "assistant", ContentJSON: rawCalls})
	for _, result := range []struct {
		id, name, status string
	}{{"failed-call", "filesystem.write", "error"}, {"success-call", "filesystem.read", "success"}} {
		payload, _ := json.Marshal(domain.ToolResult{ToolCallID: result.id, Status: result.status, Summary: result.status})
		_, _ = store.AddStructuredMessage(context.Background(), domain.Message{SessionID: task.SessionID, TaskID: taskID, Role: "tool", ToolCallID: result.id, ToolName: result.name, ContentJSON: payload})
	}
	ev, err := (&ToolResultVerifier{}).Verify(context.Background(), domain.AcceptanceCriterion{ID: "tools", TaskID: taskID}, store)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Passed {
		t.Fatal("a later successful tool call hid an earlier unresolved error")
	}
}

func TestToolResultVerifierAllowsExploratoryReadFailure(t *testing.T) {
	store, _, taskID, _ := newVerifierTask(t, "full_access")
	task, _ := store.Task(context.Background(), taskID)
	calls := []domain.ToolCall{
		{ID: "missing", Name: "filesystem.read", Arguments: map[string]any{"path": "missing.txt"}},
		{ID: "found", Name: "filesystem.read", Arguments: map[string]any{"path": "actual.txt"}},
	}
	rawCalls, _ := json.Marshal(calls)
	_, _ = store.AddStructuredMessage(context.Background(), domain.Message{SessionID: task.SessionID, TaskID: taskID, Role: "assistant", ContentJSON: rawCalls})
	results := []domain.ToolResult{
		{ToolCallID: "missing", Status: "error", Summary: "no such file", Error: &domain.ToolError{Code: "TOOL_FAILED", Message: "no such file"}},
		{ToolCallID: "found", Status: "success", Summary: "文件已读取"},
	}
	for i, result := range results {
		payload, _ := json.Marshal(result)
		_, _ = store.AddStructuredMessage(context.Background(), domain.Message{SessionID: task.SessionID, TaskID: taskID, Role: "tool", ToolCallID: result.ToolCallID, ToolName: calls[i].Name, ContentJSON: payload})
	}
	ev, err := (&ToolResultVerifier{}).Verify(context.Background(), domain.AcceptanceCriterion{ID: "tools", TaskID: taskID}, store)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Passed {
		t.Fatalf("exploratory read failure should not block completion: %+v", ev)
	}
}

func newVerifierTask(t *testing.T, mode string) (*storage.Store, *permission.Engine, string, string) {
	t.Helper()
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "verifier.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	workspace, err := store.OpenWorkspace(ctx, "test", root, "local")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, workspace.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, session.ID, "verify")
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{WorkspaceRoot: root, PermissionMode: mode, AllowShell: true}
	return store, permission.New(settings, store), task.ID, root
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

func TestVerifierFileContains(t *testing.T) {
	v := &FileContainsVerifier{}
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	cfg, _ := json.Marshal(map[string]string{"path": path, "content": "hello"})
	c := domain.AcceptanceCriterion{Type: domain.CriterionFileContains, Config: cfg, ID: "c1", TaskID: "t1"}

	ev, err := v.Verify(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Passed {
		t.Fatal("expected file_contains to find 'hello'")
	}

	cfg2, _ := json.Marshal(map[string]string{"path": path, "content": "nope"})
	c2 := domain.AcceptanceCriterion{Type: domain.CriterionFileContains, Config: cfg2, ID: "c2", TaskID: "t1"}
	ev2, err := v.Verify(context.Background(), c2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Passed {
		t.Fatal("expected file_contains to fail for 'nope'")
	}
}
