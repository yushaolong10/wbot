package contextbuilder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/history"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
)

func TestBuilderKeepsLongSessionWithinBudget(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	_ = os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600)
	s := config.Settings{ProfilePath: profile, WorkspaceRoot: root, MaxContextTokens: 8000, DefaultModel: config.Model{MaxOutputTokens: 1500}, Context: config.ContextSettings{OutputReserveTokens: 1500, SafetyMarginTokens: 500}, History: config.HistorySettings{MaxLoadedMessages: 500, RecentMessages: 12, ReactiveRecentMessages: 6, SegmentMessages: 10, SegmentMaxSourceTokens: 2000, SegmentMergeFactor: 4, SummaryTargetTokens: 400}, Memory: config.MemorySettings{Enabled: true, Retrieval: config.MemoryRetrieval{MaxEntries: 4, MaxTokens: 500, MaxEntryTokens: 200, MinScore: .2}}}
	st, err := storage.Open(filepath.Join(root, "x.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "w", root, "local")
	session, _ := st.CreateSession(ctx, w.ID, "s")
	task, _ := st.CreateTask(ctx, session.ID, "持续执行长任务")
	for i := 0; i < 100; i++ {
		_, _ = st.AddMessage(ctx, session.ID, task.ID, "user", strings.Repeat("很长的工作消息", 100))
	}
	mem := memory.New(filepath.Join(root, "memory"))
	defer mem.Close()
	hm := history.New(st, 2000, history.WithConfig(history.Config{Budget: 2000, MaxLoaded: 500, Recent: 12, ReactiveRecent: 6, SegmentMessages: 10, SegmentMaxTokens: 2000, MergeFactor: 4, SummaryTarget: 400}))
	b := New(s, st, mem, hm, func() []tool.Definition { return nil })
	got, err := b.Build(ctx, Request{SessionID: session.ID, TaskID: task.ID, Objective: task.Objective, Mode: Normal})
	if err != nil {
		t.Fatal(err)
	}
	if got.Breakdown.Total > s.MaxContextTokens {
		t.Fatalf("tokens=%d window=%d", got.Breakdown.Total, s.MaxContextTokens)
	}
	if len(got.SummaryIDs) == 0 {
		t.Fatal("expected selected summary ids")
	}
	parts := got.Breakdown.System + got.Breakdown.ToolSchemas + got.Breakdown.Task + got.Breakdown.Memory + got.Breakdown.Summary + got.Breakdown.Recent + got.Breakdown.ToolSnapshots + got.Breakdown.OutputReserve + got.Breakdown.SafetyMargin
	if parts != got.Breakdown.Total {
		t.Fatalf("breakdown parts=%d total=%d", parts, got.Breakdown.Total)
	}
}

func TestBuilderHonorsConfiguredSmallWindow(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	_ = os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600)
	s := config.Settings{ProfilePath: profile, MaxContextTokens: 4000, Context: config.ContextSettings{OutputReserveTokens: 2000, SafetyMarginTokens: 2000}}
	st, err := storage.Open(filepath.Join(root, "x.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "w", root, "local")
	session, _ := st.CreateSession(ctx, w.ID, "s")
	task, _ := st.CreateTask(ctx, session.ID, "test")
	mem := memory.New(filepath.Join(root, "memory"))
	defer mem.Close()
	b := New(s, st, mem, history.New(st, 1000), func() []tool.Definition { return nil })
	_, err = b.Build(ctx, Request{SessionID: session.ID, TaskID: task.ID, Objective: task.Objective})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestToolPairValidatorRequiresEveryResult(t *testing.T) {
	calls := []any{
		map[string]any{"id": "a"}, map[string]any{"id": "b"},
	}
	msgs := []model.Message{{Role: "assistant", ToolCalls: calls}, {Role: "tool", ToolCallID: "a", Content: "ok"}}
	if _, err := validateToolPairs(msgs); err == nil {
		t.Fatal("expected incomplete tool group to fail")
	}
	msgs = append(msgs, model.Message{Role: "tool", ToolCallID: "b", Content: "ok"})
	if _, err := validateToolPairs(msgs); err != nil {
		t.Fatal(err)
	}
}
