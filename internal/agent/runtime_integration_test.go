package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
)

type recordingNodeModel struct {
	mu    sync.Mutex
	kinds []domain.NodeKind
}

func (m *recordingNodeModel) Generate(_ context.Context, messages []model.Message, _ []tool.Definition) (model.Response, error) {
	kind := domain.NodeKind("")
	for i := len(messages) - 1; i >= 0; i-- {
		for _, candidate := range []domain.NodeKind{domain.NodeResearch, domain.NodePlan, domain.NodeExecute} {
			if strings.Contains(fmt.Sprint(messages[i].Content), "kind="+string(candidate)) {
				kind = candidate
				break
			}
		}
		if kind != "" {
			break
		}
	}
	if kind == "" {
		return model.Response{}, errors.New("current node instruction missing")
	}
	m.mu.Lock()
	m.kinds = append(m.kinds, kind)
	m.mu.Unlock()
	return model.Response{Content: string(kind) + " completed"}, nil
}

func (m *recordingNodeModel) observed() []domain.NodeKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.NodeKind(nil), m.kinds...)
}

type failingCompletionGate struct{}

func (g *failingCompletionGate) Evaluate(context.Context, domain.Task, string) (domain.GateResult, error) {
	return domain.GateResult{}, errors.New("evidence database unavailable")
}

func (g *failingCompletionGate) CompleteTask(context.Context, string, string) error {
	return errors.New("must not be called")
}

type replanOnceGate struct {
	store *storage.Store
	mu    sync.Mutex
	calls int
}

type fixedActionGate struct {
	store  *storage.Store
	action domain.GateAction
}

func (g *fixedActionGate) Evaluate(context.Context, domain.Task, string) (domain.GateResult, error) {
	return domain.GateResult{Action: g.action, Reason: "fixed gate result"}, nil
}

func (g *fixedActionGate) CompleteTask(ctx context.Context, taskID, result string) error {
	return g.store.UpdateTask(ctx, taskID, "completed", result, "")
}

func (g *replanOnceGate) Evaluate(context.Context, domain.Task, string) (domain.GateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls == 1 {
		return domain.GateResult{Action: domain.ActionReplan, Reason: "new evidence invalidated the plan"}, nil
	}
	return domain.GateResult{Action: domain.ActionComplete, Reason: "replanned graph verified"}, nil
}

func (g *replanOnceGate) CompleteTask(ctx context.Context, taskID, result string) error {
	return g.store.UpdateTask(ctx, taskID, "completed", result, "")
}

func newRuntimeTestService(t *testing.T, generator model.Generator) (*Service, *storage.Store, *memory.Manager, domain.Session) {
	t.Helper()
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	if err := os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "full_access", AllowShell: true, ProfilePath: profile, MaxParallelism: 2, MaxContextTokens: 20000, Context: config.ContextSettings{OutputReserveTokens: 1000, SafetyMarginTokens: 500}, Memory: config.MemorySettings{Enabled: false}}
	store, err := storage.Open(settings.DatabasePath, root)
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New(filepath.Join(root, "memory"), memory.WithConfig(memory.ConfigFrom(settings.Memory)))
	registry := tool.New(settings, store, permission.New(settings, store), mem, nil)
	service := New(settings, store, generator, registry, mem)
	workspace, err := store.OpenWorkspace(context.Background(), "test", root, "local")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), workspace.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return service, store, mem, session
}

func waitTaskTerminal(t *testing.T, store *storage.Store, taskID string) domain.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := store.Task(context.Background(), taskID)
		if err == nil && (task.Status == "completed" || task.Status == "failed") {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := store.Task(context.Background(), taskID)
	t.Fatalf("task did not finish: %+v", task)
	return domain.Task{}
}

func TestComplexGraphExecutesEveryDependencyInOrder(t *testing.T) {
	generator := &recordingNodeModel{}
	service, store, mem, session := newRuntimeTestService(t, generator)
	defer store.Close()
	defer mem.Close()
	task, err := service.Start(context.Background(), session.ID, "实现一个完整系统并重构核心模块")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskTerminal(t, store, task.ID)
	if finished.Status != "completed" {
		t.Fatalf("task failed: %s", finished.Error)
	}
	want := []domain.NodeKind{domain.NodeResearch, domain.NodePlan, domain.NodeExecute}
	if got := generator.observed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("execution order=%v want=%v", got, want)
	}
	nodes, err := store.Nodes(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Status != "completed" {
			t.Fatalf("node was skipped: %+v", node)
		}
	}
}

func TestCompletionGateErrorFailsClosed(t *testing.T) {
	generator := &recordingNodeModel{}
	service, store, mem, session := newRuntimeTestService(t, generator)
	defer store.Close()
	defer mem.Close()
	service.gate = &failingCompletionGate{}
	task, err := service.Start(context.Background(), session.ID, "回答当前问题")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskTerminal(t, store, task.ID)
	if finished.Status != "failed" || !strings.Contains(finished.Error, "completion gate failed closed") {
		t.Fatalf("gate failure was not closed: %+v", finished)
	}
}

func TestCompletionGateFailDoesNotReplan(t *testing.T) {
	generator := &recordingNodeModel{}
	service, store, mem, session := newRuntimeTestService(t, generator)
	defer store.Close()
	defer mem.Close()
	service.gate = &fixedActionGate{store: store, action: domain.ActionFail}
	task, err := service.Start(context.Background(), session.ID, "回答当前问题")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskTerminal(t, store, task.ID)
	if finished.Status != "failed" {
		t.Fatalf("gate fail did not fail task: %+v", finished)
	}
	revisions, err := store.GraphRevisions(context.Background(), task.ID)
	if err != nil || len(revisions) != 1 {
		t.Fatalf("gate fail unexpectedly replanned: revisions=%+v err=%v", revisions, err)
	}
}

func TestCompletionGateRetryIsBoundedWithoutReplan(t *testing.T) {
	generator := &recordingNodeModel{}
	service, store, mem, session := newRuntimeTestService(t, generator)
	defer store.Close()
	defer mem.Close()
	service.gate = &fixedActionGate{store: store, action: domain.ActionRetry}
	task, err := service.Start(context.Background(), session.ID, "回答当前问题")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskTerminal(t, store, task.ID)
	if finished.Status != "failed" || !strings.Contains(finished.Error, "retry limit") {
		t.Fatalf("bounded retry did not fail as expected: %+v", finished)
	}
	revisions, err := store.GraphRevisions(context.Background(), task.ID)
	if err != nil || len(revisions) != 1 {
		t.Fatalf("retry unexpectedly created graph revisions: revisions=%+v err=%v", revisions, err)
	}
}

func TestReplanReplacesPendingGraphAndCompletesNextRevision(t *testing.T) {
	generator := &recordingNodeModel{}
	service, store, mem, session := newRuntimeTestService(t, generator)
	defer store.Close()
	defer mem.Close()
	service.gate = &replanOnceGate{store: store}
	task, err := service.Start(context.Background(), session.ID, "回答当前问题")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskTerminal(t, store, task.ID)
	if finished.Status != "completed" {
		t.Fatalf("replanned task did not complete: %+v", finished)
	}
	revisions, err := store.GraphRevisions(context.Background(), task.ID)
	if err != nil || len(revisions) != 2 || revisions[1].Version != 2 {
		t.Fatalf("revisions=%+v err=%v", revisions, err)
	}
	var revisionNodes []domain.Node
	if err = json.Unmarshal([]byte(revisions[1].NodesJSON), &revisionNodes); err != nil || len(revisionNodes) == 0 {
		t.Fatalf("revision nodes are not materialized: nodes=%+v err=%v", revisionNodes, err)
	}
	for _, node := range revisionNodes {
		if node.ID == "" {
			t.Fatalf("revision node is missing durable ID: %+v", node)
		}
	}
	nodes, err := store.Nodes(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Status != "completed" {
			t.Fatalf("unfinished node after replan: %+v", node)
		}
	}
	var firstRevisionNodes []domain.Node
	if err = json.Unmarshal([]byte(revisions[0].NodesJSON), &firstRevisionNodes); err != nil {
		t.Fatal(err)
	}
	initialContextIDs := map[domain.NodeKind]string{}
	for _, node := range firstRevisionNodes {
		if node.Kind == domain.NodeLoadHist || node.Kind == domain.NodeRetrieve {
			initialContextIDs[node.Kind] = node.ID
		}
	}
	for _, node := range revisionNodes {
		if want := initialContextIDs[node.Kind]; want != "" && node.ID != want {
			t.Fatalf("completed context node %s was replaced: got=%s want=%s", node.Kind, node.ID, want)
		}
	}
	criteria, err := store.AcceptanceCriteria(context.Background(), task.ID)
	if err != nil || len(criteria) != 2 {
		t.Fatalf("replacement criteria=%+v err=%v", criteria, err)
	}
}

func TestReplanPreservesOriginalObjectiveAndRequiredCriteria(t *testing.T) {
	service, store, mem, session := newRuntimeTestService(t, &recordingNodeModel{})
	defer store.Close()
	defer mem.Close()

	ctx := context.Background()
	task, err := store.CreateTask(ctx, session.ID, "修复原始目标中的关键缺陷")
	if err != nil {
		t.Fatal(err)
	}
	nodes, criteria, revision, err := service.planner.GenerateGraph(ctx, task, "complex")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CreateGraphWithCriteria(ctx, task.ID, nodes, criteria, revision); err != nil {
		t.Fatal(err)
	}

	custom := domain.AcceptanceCriterion{
		ID:          "ac_required_contract",
		TaskID:      task.ID,
		NodeID:      nodes[len(nodes)-2].ID,
		Type:        domain.CriterionCommand,
		Description: "Go 1.21 tests must pass",
		Required:    true,
		Config:      json.RawMessage(`{"command":"go test ./..."}`),
		Status:      "failed",
		Reason:      "tests failed before replan",
	}
	if err = store.CreateAcceptanceCriterion(ctx, custom); err != nil {
		t.Fatal(err)
	}

	replannedNodes, replannedCriteria, replannedRevision, err := service.planner.Replan(ctx, task, "verify node failed")
	if err != nil {
		t.Fatal(err)
	}
	var executeNode domain.Node
	for _, node := range replannedNodes {
		if node.Kind == domain.NodeExecute {
			executeNode = node
			break
		}
	}
	if executeNode.Description != task.Objective {
		t.Fatalf("replan objective=%q want original objective %q", executeNode.Description, task.Objective)
	}

	var retained *domain.AcceptanceCriterion
	for i := range replannedCriteria {
		if replannedCriteria[i].ID == custom.ID {
			retained = &replannedCriteria[i]
			break
		}
	}
	if retained == nil || !retained.Required || retained.Type != custom.Type || retained.Description != custom.Description || string(retained.Config) != string(custom.Config) {
		t.Fatalf("required criterion was not preserved: %+v", retained)
	}
	if retained.NodeID != executeNode.ID || !containsString(executeNode.CriteriaIDs, custom.ID) {
		t.Fatalf("required criterion was not rebound to execute node: criterion=%+v node=%+v", retained, executeNode)
	}

	if err = store.ReplaceUnfinishedGraph(ctx, task.ID, replannedNodes, replannedCriteria, replannedRevision); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.AcceptanceCriteria(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, criterion := range persisted {
		if criterion.ID == custom.ID {
			found = criterion.Required && criterion.Type == custom.Type && criterion.Description == custom.Description
		}
	}
	if !found {
		t.Fatalf("required criterion missing after graph replacement: %+v", persisted)
	}

	withoutRequired := make([]domain.AcceptanceCriterion, 0, len(replannedCriteria)-1)
	for _, criterion := range replannedCriteria {
		if criterion.ID != custom.ID {
			withoutRequired = append(withoutRequired, criterion)
		}
	}
	if err = store.ReplaceUnfinishedGraph(ctx, task.ID, replannedNodes, withoutRequired, replannedRevision); err == nil || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("store accepted removal of required criterion: %v", err)
	}

	weakened := append([]domain.AcceptanceCriterion(nil), replannedCriteria...)
	for i := range weakened {
		if weakened[i].ID == custom.ID {
			weakened[i].Required = false
			break
		}
	}
	if err = store.ReplaceUnfinishedGraph(ctx, task.ID, replannedNodes, weakened, replannedRevision); err == nil || !strings.Contains(err.Error(), "was weakened") {
		t.Fatalf("store accepted weakening of required criterion: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
