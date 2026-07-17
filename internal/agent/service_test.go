package agent

import (
	"context"
	"fmt"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeModel struct {
	mu sync.Mutex
	n  int
}
type approvalModel struct {
	mu sync.Mutex
	n  int
}

func (f *approvalModel) Generate(_ context.Context, _ []model.Message, _ []tool.Definition) (model.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n <= 2 {
		return model.Response{ToolCalls: []domain.ToolCall{{ID: fmt.Sprintf("s%d", f.n), Name: "shell.execute", Arguments: map[string]any{"command": "printf approved > approved.txt"}}}}, nil
	}
	return model.Response{Content: "审批后的操作已执行并验证。"}, nil
}

func (f *fakeModel) Generate(_ context.Context, _ []model.Message, _ []tool.Definition) (model.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == 1 {
		return model.Response{ToolCalls: []domain.ToolCall{{ID: "c1", Name: "filesystem.write", Arguments: map[string]any{"path": "done.txt", "content": "ok"}}}}, nil
	}
	return model.Response{Content: "任务已完成，并已写入 done.txt。"}, nil
}
func TestRunToolAndComplete(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600)
	s := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "approval", AllowShell: true, ProfilePath: profile}
	st, e := storage.Open(s.DatabasePath, root)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	mem := memory.New(filepath.Join(root, "memory"))
	os.MkdirAll(filepath.Join(root, "memory"), 0700)
	p := permission.New(s, st)
	reg := tool.New(s, st, p, mem, nil)
	svc := New(s, st, &fakeModel{}, reg, mem)
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "x", root, "local")
	sess, _ := st.CreateSession(ctx, w.ID, "x")
	task, e := svc.Start(ctx, sess.ID, "写文件")
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := st.Task(ctx, task.ID)
		if got.Status == "completed" {
			b, e := os.ReadFile(filepath.Join(root, "done.txt"))
			if e != nil || string(b) != "ok" {
				t.Fatalf("file: %q %v", b, e)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not complete")
}
func TestApprovalPauseAndResume(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600)
	s := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "approval", AllowShell: true, ProfilePath: profile, MaxParallelism: 2, MaxContextTokens: 4000}
	st, e := storage.Open(s.DatabasePath, root)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	memRoot := filepath.Join(root, "memory")
	os.MkdirAll(memRoot, 0700)
	mem := memory.New(memRoot)
	reg := tool.New(s, st, permission.New(s, st), mem, nil)
	svc := New(s, st, &approvalModel{}, reg, mem)
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "x", root, "local")
	sess, _ := st.CreateSession(ctx, w.ID, "x")
	task, _ := svc.Start(ctx, sess.ID, "审批测试")
	waitStatus := func(want string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			got, _ := st.Task(ctx, task.ID)
			if got.Status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task did not reach %s", want)
	}
	waitStatus("waiting_approval")
	as, _ := st.Approvals(ctx, "pending")
	if len(as) != 1 {
		t.Fatalf("approvals=%d", len(as))
	}
	st.DecideApproval(ctx, as[0].ID, "approved")
	svc.Resume(ctx, task.ID)
	waitStatus("completed")
	if b, e := os.ReadFile(filepath.Join(root, "approved.txt")); e != nil || string(b) != "approved" {
		t.Fatalf("side effect missing: %q %v", b, e)
	}
}
