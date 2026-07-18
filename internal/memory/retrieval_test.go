package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type mergeModel struct{}

func (mergeModel) Complete(_ context.Context, system, user string) (string, error) {
	if strings.Contains(system, "关系") {
		var v struct {
			Existing []Entry `json:"existing"`
		}
		_ = json.Unmarshal([]byte(user), &v)
		if len(v.Existing) > 0 {
			return `{"action":"merge","target_id":"` + v.Existing[0].ID + `"}`, nil
		}
	}
	return `{"selected":[]}`, nil
}

type actionModel struct{ action string }

func (a actionModel) Complete(_ context.Context, system, user string) (string, error) {
	if strings.Contains(system, "关系") {
		var v struct {
			Existing []Entry `json:"existing"`
		}
		_ = json.Unmarshal([]byte(user), &v)
		if len(v.Existing) > 0 {
			return `{"action":"` + a.action + `","target_id":"` + v.Existing[0].ID + `"}`, nil
		}
	}
	return `{"selected":[]}`, nil
}

func TestChineseFTSRetrievalBudgetAndConsolidation(t *testing.T) {
	m := New(t.TempDir(), WithGenerator(mergeModel{}))
	defer m.Close()
	ctx := context.Background()
	if err := m.Upsert(ctx, Entry{Type: "project", Summary: "上下文管理方案", Content: "系统使用分段摘要避免消息历史持续膨胀", Tags: []string{"上下文", "摘要"}, Confidence: .9, Importance: .8}); err != nil {
		t.Fatal(err)
	}
	got, err := m.RetrieveQuery(ctx, Query{Text: "消息历史分段摘要", MaxEntries: 4, MaxTokens: 500, MaxEntryTokens: 200, MinScore: .1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("retrieved=%d", len(got))
	}
	if err = m.Upsert(ctx, Entry{Type: "project", Summary: "历史消息压缩", Content: "通过分段摘要控制上下文增长", Tags: []string{"上下文"}, Confidence: .95, Importance: .9}); err != nil {
		t.Fatal(err)
	}
	all, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Version != 2 {
		t.Fatalf("memories=%d version=%v", len(all), all)
	}
}

func TestProjectTypeScopeAndConflictAreExplicit(t *testing.T) {
	m := New(t.TempDir(), WithGenerator(actionModel{action: "mark_conflict"}))
	defer m.Close()
	ctx := context.Background()
	if err := m.Upsert(ctx, Entry{ProjectScope: "p1", Type: "project", Summary: "database choice", Content: "use sqlite database", Tags: []string{"database"}, Confidence: .9, Importance: .8}); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(ctx, Entry{ProjectScope: "p1", Type: "project", Summary: "database choice changed", Content: "use postgres database", Tags: []string{"database"}, Confidence: .9, Importance: .8}); err != nil {
		t.Fatal(err)
	}
	got, err := m.RetrieveQuery(ctx, Query{Text: "database choice", ProjectScope: "p1", DesiredTypes: []string{"project"}, MaxEntries: 8, MaxTokens: 1000, MaxEntryTokens: 300, MinScore: .01})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got=%d", len(got))
	}
	if len(got[0].ConflictIDs) == 0 || len(got[1].ConflictIDs) == 0 {
		t.Fatalf("conflicts not exposed: %+v", got)
	}
	other, err := m.RetrieveQuery(ctx, Query{Text: "database choice", ProjectScope: "p2", MaxEntries: 8, MaxTokens: 1000, MaxEntryTokens: 300, MinScore: .01})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-project results=%v err=%v", other, err)
	}
}

func TestReplaceCreatesSupersedingMemory(t *testing.T) {
	m := New(t.TempDir(), WithGenerator(actionModel{action: "replace"}))
	defer m.Close()
	ctx := context.Background()
	first := Entry{Type: "project", Summary: "runtime version", Content: "project uses Go 1.22", Tags: []string{"runtime"}, Confidence: .9, Importance: .8}
	if err := m.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(ctx, Entry{Type: "project", Summary: "runtime version", Content: "project now uses Go 1.24", Tags: []string{"runtime"}, Confidence: .95, Importance: .9}); err != nil {
		t.Fatal(err)
	}
	all, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("entries=%d", len(all))
	}
	statuses := map[string]int{}
	for _, x := range all {
		statuses[x.Status]++
	}
	if statuses["active"] != 1 || statuses["archived"] != 1 {
		t.Fatalf("statuses=%v", statuses)
	}
	var links int
	if err = m.db.QueryRow(`SELECT COUNT(*) FROM memory_links WHERE relation='supersedes'`).Scan(&links); err != nil || links != 1 {
		t.Fatalf("links=%d err=%v", links, err)
	}
}

func TestMaintenancePurgesDeletedAndTrimsVersions(t *testing.T) {
	m := New(t.TempDir(), WithConfig(Config{Enabled: true, UseFTS: true, VersionRetentionCount: 2, DeletedRetentionDays: 1, EnablePhysicalGC: true}))
	defer m.Close()
	ctx := context.Background()
	e := Entry{Type: "project", Summary: "stable fact", Content: "value one", Confidence: .9, Importance: .8}
	if err := m.Upsert(ctx, e); err != nil {
		t.Fatal(err)
	}
	all, _ := m.List(ctx)
	e = all[0]
	for _, value := range []string{"value two", "value three", "value four"} {
		e.Content = value
		if err := m.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339Nano)
	if _, err := m.db.Exec(`UPDATE memories SET updated_at=? WHERE id=?`, old, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var memories, versions int
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, e.ID).Scan(&memories)
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM memory_versions WHERE memory_id=?`, e.ID).Scan(&versions)
	if memories != 0 || versions != 0 {
		t.Fatalf("memories=%d versions=%d", memories, versions)
	}
}
