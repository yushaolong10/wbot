package tool

import (
	"context"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"os"
	"path/filepath"
	"testing"
)

type advisorStub struct{ n int }

func (a *advisorStub) Consult(context.Context, string, string) (string, error) {
	a.n++
	return "advice", nil
}
func TestIdempotencyAndAdvisorLimit(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{WorkspaceRoot: root, PermissionMode: "approval", AdvisorMaxCalls: 1}
	st, e := storage.Open(filepath.Join(root, "x.db"), root)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	memRoot := filepath.Join(root, "memory")
	os.MkdirAll(memRoot, 0700)
	a := &advisorStub{}
	r := New(s, st, permission.New(s, st), memory.New(memRoot), a)
	ctx := context.Background()
	call := domain.ToolCall{ID: "c1", Name: "filesystem.write", Arguments: map[string]any{"path": "x.txt", "content": "one"}}
	got, _ := r.Execute(ctx, "task", call)
	if got.Status != "success" {
		t.Fatal(got)
	}
	os.WriteFile(filepath.Join(root, "x.txt"), []byte("external"), 0600)
	call.ID = "c2"
	got, _ = r.Execute(ctx, "task", call)
	b, _ := os.ReadFile(filepath.Join(root, "x.txt"))
	if string(b) != "external" || got.Status != "success" {
		t.Fatalf("duplicate re-executed: %q %#v", b, got)
	}
	adv := domain.ToolCall{ID: "a1", Name: "consult_advisor", Arguments: map[string]any{"problem": "p1", "expected_output": "e"}}
	got, _ = r.Execute(ctx, "task", adv)
	if got.Status != "success" {
		t.Fatal(got)
	}
	adv.ID = "a2"
	adv.Arguments["problem"] = "p2"
	got, _ = r.Execute(ctx, "task", adv)
	if got.Status != "error" || a.n != 1 {
		t.Fatalf("advisor limit failed: %#v calls=%d", got, a.n)
	}
}
