package history

import (
	"context"
	"encoding/json"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactionPersistsSummary(t *testing.T) {
	root := t.TempDir()
	st, e := storage.Open(filepath.Join(root, "x.db"), root)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "w", root, "local")
	s, _ := st.CreateSession(ctx, w.ID, "s")
	for i := 0; i < 30; i++ {
		st.AddMessage(ctx, s.ID, "", "user", strings.Repeat("长消息", 200))
	}
	m := New(st, 1000)
	summary, recent, e := m.Select(ctx, s.ID)
	if e != nil || summary == "" || len(recent) != 12 {
		t.Fatalf("summary=%d recent=%d err=%v", len(summary), len(recent), e)
	}
	summary2, _, _ := m.Select(ctx, s.ID)
	if summary2 != summary {
		t.Fatal("same range was summarized twice")
	}
}

func TestCompactSegmentsRetainsEverySummaryField(t *testing.T) {
	summary := domain.HistorySummary{
		Objectives: []string{"objective"}, UserConstraints: []string{"constraint"}, VerifiedFacts: []string{"fact"},
		Decisions: []string{"decision"}, CompletedActions: []string{"completed"}, PendingActions: []string{"pending"},
		FailedActions: []string{"failed"}, ActiveToolCalls: []string{"call"}, Artifacts: []string{"artifact"},
		MemoryIDs: []string{"memory"}, FileChanges: []string{"file"}, OpenQuestions: []string{"question"},
	}
	raw, _ := json.Marshal(summary)
	compact := compactSegmentsText([]domain.HistorySegment{{SummaryJSON: string(raw)}})
	for _, value := range []string{"constraint", "fact", "call", "artifact", "memory", "file", "question"} {
		if !strings.Contains(compact, value) {
			t.Fatalf("compact summary dropped %q: %s", value, compact)
		}
	}
}
