package model

import (
	"context"
	"encoding/json"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/tool"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAICompatibilityAndRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(429)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "test" || body["reasoning_effort"] != "max" {
			t.Errorf("unexpected body %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":2}}`))
	}))
	defer srv.Close()
	c := New(config.Model{BaseURL: srv.URL, Name: "test", APIKey: "key", Timeout: 5 * time.Second, MaxOutputTokens: 10, Thinking: "enabled", ReasoningEffort: "max"})
	r, e := c.Generate(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if e != nil || r.Content != "ok" || calls != 2 {
		t.Fatalf("result=%#v err=%v calls=%d", r, e, calls)
	}
}

func TestToolNamesAreMappedForCompatibleAPIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "filesystem_read" {
			t.Fatalf("unexpected model tool name: %#v", body.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"filesystem_read","arguments":"{\"path\":\"README.md\"}"}}]}}]}`))
	}))
	defer srv.Close()

	c := New(config.Model{BaseURL: srv.URL, Name: "test", APIKey: "key", Timeout: 5 * time.Second})
	defs := []tool.Definition{{Name: "filesystem.read", Description: "read", Parameters: map[string]any{"type": "object"}}}
	response, err := c.Generate(context.Background(), []Message{{Role: "user", Content: "read"}}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "filesystem.read" {
		t.Fatalf("tool name was not restored: %#v", response.ToolCalls)
	}
}
