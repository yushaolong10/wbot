package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/wbot-dev/wbot/internal/domain"
)

func TestStructuredMessagesAndDurableMaintenance(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "x.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	w, _ := s.OpenWorkspace(ctx, "w", root, "local")
	session, _ := s.CreateSession(ctx, w.ID, "s")
	calls, _ := json.Marshal([]domain.ToolCall{{ID: "call1", Name: "shell.execute", Arguments: map[string]any{"command": "true"}}})
	a, err := s.AddStructuredMessage(ctx, domain.Message{SessionID: session.ID, Role: "assistant", ContentJSON: calls})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(domain.ToolResult{ToolCallID: "call1", Status: "success"})
	b, err := s.AddStructuredMessage(ctx, domain.Message{SessionID: session.ID, Role: "tool", ContentJSON: raw, ToolCallID: "call1", ToolName: "shell.execute"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != 1 || b.Seq != 2 || a.TokenCount == 0 || b.ContentHash == "" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if err = s.EnqueueMaintenance(ctx, "memory.extract", "task1", map[string]string{"task_id": "task1"}); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.ClaimMaintenance(ctx, time.Minute)
	if err != nil || !ok || job.Kind != "memory.extract" {
		t.Fatalf("job=%+v ok=%v err=%v", job, ok, err)
	}
	if err = s.FinishMaintenance(ctx, job.ID, nil); err != nil {
		t.Fatal(err)
	}
}
