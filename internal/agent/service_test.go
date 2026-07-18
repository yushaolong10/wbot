package agent

import (
	"context"
	"encoding/json"
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
	"strings"
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
type overflowModel struct {
	mu sync.Mutex
	n  int
}

func (f *overflowModel) Generate(context.Context, []model.Message, []tool.Definition) (model.Response, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return model.Response{}, fmt.Errorf("maximum token context exceeded")
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
			criteria, err := st.AcceptanceCriteria(ctx, task.ID)
			if err != nil || len(criteria) != 2 {
				t.Fatalf("criteria=%+v err=%v", criteria, err)
			}
			for _, criterion := range criteria {
				if criterion.ID == "" || criterion.NodeID == "" || criterion.Status != "passed" || len(criterion.EvidenceIDs) == 0 {
					t.Fatalf("criterion not connected to evidence: %+v", criterion)
				}
			}
			evidence, err := st.EvidenceByTask(ctx, task.ID)
			if err != nil || len(evidence) != 2 {
				t.Fatalf("evidence=%+v err=%v", evidence, err)
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
	s := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "approval", AllowShell: true, ProfilePath: profile, MaxParallelism: 2, MaxContextTokens: 8000, Context: config.ContextSettings{OutputReserveTokens: 1000, SafetyMarginTokens: 500}}
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
		var last domain.Task
		for time.Now().Before(deadline) {
			got, _ := st.Task(ctx, task.ID)
			last = got
			if got.Status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task did not reach %s: status=%s error=%s", want, last.Status, last.Error)
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

func TestSecondContextOverflowReturnsBudgetError(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	_ = os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600)
	s := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "full_access", AllowShell: true, ProfilePath: profile, MaxParallelism: 1, MaxContextTokens: 8000, Context: config.ContextSettings{OutputReserveTokens: 1000, SafetyMarginTokens: 500}, History: config.HistorySettings{RecentMessages: 20, ReactiveRecentMessages: 6}, Memory: config.MemorySettings{Enabled: false}}
	st, err := storage.Open(s.DatabasePath, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := memory.New(filepath.Join(root, "memory"), memory.WithConfig(memory.ConfigFrom(s.Memory)))
	defer mem.Close()
	fm := &overflowModel{}
	svc := New(s, st, fm, tool.New(s, st, permission.New(s, st), mem, nil), mem)
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "x", root, "local")
	sess, _ := st.CreateSession(ctx, w.ID, "x")
	task, _ := svc.Start(ctx, sess.ID, "overflow")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := st.Task(ctx, task.ID)
		if got.Status == "failed" {
			if !strings.Contains(got.Error, "context budget exceeded after reactive retry") {
				t.Fatalf("error=%s", got.Error)
			}
			fm.mu.Lock()
			calls := fm.n
			fm.mu.Unlock()
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not fail")
}

func TestInterruptedToolGroupGetsUnknownResult(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{WorkspaceRoot: root, PermissionMode: "full_access", Memory: config.MemorySettings{Enabled: false}}
	st, err := storage.Open(filepath.Join(root, "w.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := memory.New(filepath.Join(root, "memory"), memory.WithConfig(memory.ConfigFrom(s.Memory)))
	defer mem.Close()
	svc := New(s, st, &fakeModel{}, tool.New(s, st, permission.New(s, st), mem, nil), mem)
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "x", root, "local")
	sess, _ := st.CreateSession(ctx, w.ID, "x")
	task, _ := st.CreateTask(ctx, sess.ID, "recover")
	calls := []domain.ToolCall{{ID: "a", Name: "filesystem.read", Arguments: map[string]any{"path": "a"}}, {ID: "b", Name: "filesystem.read", Arguments: map[string]any{"path": "b"}}}
	rawCalls, _ := json.Marshal(calls)
	_, _ = st.AddStructuredMessage(ctx, domain.Message{SessionID: sess.ID, TaskID: task.ID, Role: "assistant", ContentJSON: rawCalls})
	first, _ := json.Marshal(domain.ToolResult{ToolCallID: "a", Status: "success"})
	_, _ = st.AddStructuredMessage(ctx, domain.Message{SessionID: sess.ID, TaskID: task.ID, Role: "tool", ToolCallID: "a", ContentJSON: first})
	if err = svc.closeInterruptedToolGroup(ctx, task); err != nil {
		t.Fatal(err)
	}
	msgs, _ := st.Messages(ctx, sess.ID, 10)
	if len(msgs) != 3 || msgs[2].ToolCallID != "b" || !strings.Contains(string(msgs[2].ContentJSON), "RESULT_UNKNOWN") {
		t.Fatalf("messages=%+v", msgs)
	}
}

func TestExecuteToolBatchIsBoundedAndPreservesOrder(t *testing.T) {
	calls := make([]domain.ToolCall, 5)
	for index := range calls {
		calls[index] = domain.ToolCall{ID: fmt.Sprintf("read-%d", index), Name: "filesystem.read"}
	}
	entered := make(chan struct{}, len(calls))
	release := make(chan struct{})
	done := make(chan []toolExecution, 1)
	go func() {
		done <- executeToolBatch(context.Background(), calls, 3, func(_ context.Context, call domain.ToolCall) toolExecution {
			entered <- struct{}{}
			<-release
			return toolExecution{call: call, result: domain.ToolResult{ToolCallID: call.ID}}
		})
	}()
	for index := 0; index < 3; index++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("batch did not start configured workers")
		}
	}
	select {
	case <-entered:
		t.Fatal("batch exceeded concurrency limit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	var results []toolExecution
	select {
	case results = <-done:
	case <-time.After(time.Second):
		t.Fatal("batch did not complete")
	}
	if len(results) != len(calls) {
		t.Fatalf("results=%d", len(results))
	}
	for index, result := range results {
		if result.call.ID != calls[index].ID || result.result.ToolCallID != calls[index].ID {
			t.Fatalf("result %d out of order: %+v", index, result)
		}
	}
}
