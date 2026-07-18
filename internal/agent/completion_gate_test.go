package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
)

func TestCompletionGateMigratesLegacyCriteriaBeforeSavingEvidence(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(filepath.Join(root, "gate.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	workspace, err := store.OpenWorkspace(ctx, "test", root, "local")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, workspace.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, session.ID, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetCriterion(ctx, task.ID, "模型返回非空交付结果", true, "non-empty"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetCriterion(ctx, task.ID, "不存在未处理的工具错误", true, "clean"); err != nil {
		t.Fatal(err)
	}
	collector := NewEvidenceCollector(store, NewVerifierRegistry())
	gate := NewCompletionGate(store, collector)
	result, err := gate.Evaluate(ctx, task, "legacy-node")
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != domain.ActionComplete {
		t.Fatalf("gate result=%+v", result)
	}
	criteria, err := store.AcceptanceCriteria(ctx, task.ID)
	if err != nil || len(criteria) != 2 {
		t.Fatalf("criteria=%+v err=%v", criteria, err)
	}
	evidence, err := store.EvidenceByTask(ctx, task.ID)
	if err != nil || len(evidence) != 2 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	for _, item := range evidence {
		found := false
		for _, criterion := range criteria {
			if item.CriterionID == criterion.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("evidence references missing criterion: %+v", item)
		}
	}
}
