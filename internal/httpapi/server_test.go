package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wbot-dev/wbot/internal/agent"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
)

type blockingModel struct {
	started chan struct{}
}

func (m *blockingModel) Generate(ctx context.Context, _ []model.Message, _ []tool.Definition) (model.Response, error) {
	select {
	case m.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return model.Response{}, ctx.Err()
}

func TestCancelEndpointStopsRunningTask(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.yaml")
	if err := os.WriteFile(profile, []byte("version: 1\nidentity:\n  name: wbot\n  role: test\n  language: zh-CN\npersonality:\n  tone: direct\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{DataRoot: root, DatabasePath: filepath.Join(root, "w.db"), WorkspaceRoot: root, PermissionMode: "full_access", ProfilePath: profile, MaxParallelism: 2, MaxContextTokens: 8000, Context: config.ContextSettings{OutputReserveTokens: 1000, SafetyMarginTokens: 500}, Memory: config.MemorySettings{Enabled: false}}
	store, err := storage.Open(settings.DatabasePath, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mem := memory.New(filepath.Join(root, "memory"), memory.WithConfig(memory.ConfigFrom(settings.Memory)))
	defer mem.Close()
	modelClient := &blockingModel{started: make(chan struct{}, 1)}
	registry := tool.New(settings, store, permission.New(settings, store), mem, nil)
	agentService := agent.New(settings, store, modelClient, registry, mem)
	server := New(settings, store, agentService, mem)
	ctx := context.Background()
	workspace, _ := store.OpenWorkspace(ctx, "test", root, "local")
	session, _ := store.CreateSession(ctx, workspace.ID, "test")
	task, err := agentService.Start(ctx, session.ID, "等待取消")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-modelClient.started:
	case <-time.After(2 * time.Second):
		t.Fatal("model call did not start")
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var cancelled struct{ Status string }
	if err = json.Unmarshal(response.Body.Bytes(), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("response status=%q", cancelled.Status)
	}
	stored, err := store.Task(ctx, task.ID)
	if err != nil || stored.Status != "cancelled" {
		t.Fatalf("stored task=%+v err=%v", stored, err)
	}
}
