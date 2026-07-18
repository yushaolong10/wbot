package history

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
)

func TestHierarchicalSegmentsCoverHistoryWithoutGaps(t *testing.T) {
	root := t.TempDir()
	st, err := storage.Open(filepath.Join(root, "x.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	w, _ := st.OpenWorkspace(ctx, "w", root, "local")
	s, _ := st.CreateSession(ctx, w.ID, "long")
	for i := 0; i < 180; i++ {
		_, _ = st.AddMessage(ctx, s.ID, "", "user", "历史消息")
	}
	m := New(st, 1000, WithConfig(Config{Budget: 1000, MaxLoaded: 100, Recent: 12, SegmentMessages: 10, SegmentMaxTokens: 2000, MergeFactor: 4, SummaryTarget: 400}))
	summary, recent, err := m.Select(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary == "" || len(recent) != 12 {
		t.Fatalf("summary=%d recent=%d", len(summary), len(recent))
	}
	active, err := st.HistorySegments(ctx, s.ID, "active")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].FirstSeq < active[j].FirstSeq })
	want, cutoff := int64(1), recent[0].Seq-1
	for _, seg := range active {
		if seg.FirstSeq != want {
			t.Fatalf("coverage gap/overlap: want %d got %d-%d", want, seg.FirstSeq, seg.LastSeq)
		}
		want = seg.LastSeq + 1
	}
	if want-1 != cutoff {
		t.Fatalf("coverage ended at %d want %d", want-1, cutoff)
	}
}

func TestDeterministicSummaryDoesNotVerifyAssistantClaims(t *testing.T) {
	s := deterministicSummary([]domain.Message{{Role: "assistant", Content: "I think deployment succeeded"}})
	if len(s.VerifiedFacts) != 0 || len(s.PendingActions) != 1 {
		t.Fatalf("summary=%+v", s)
	}
}

func TestRenderFrontierKeepsEveryActiveRange(t *testing.T) {
	m := &Manager{}
	segments := []domain.HistorySegment{{ID: "a", FirstSeq: 1, LastSeq: 10, SummaryJSON: `{"objectives":["a"]}`}, {ID: "b", FirstSeq: 11, LastSeq: 20, SummaryJSON: `{"objectives":["b"]}`}}
	_, ids := m.renderFrontier(segments, 10)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids=%v", ids)
	}
}
