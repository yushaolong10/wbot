package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/tool"
)

type Message struct {
	Role       string `json:"role"`
	Content    any    `json:"content,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  any    `json:"tool_calls,omitempty"`
}
type Response struct {
	Content   string
	ToolCalls []domain.ToolCall
	Usage     map[string]any
}
type Generator interface {
	Generate(context.Context, []Message, []tool.Definition) (Response, error)
}
type Client struct {
	cfg  config.Model
	http *http.Client
}

func New(c config.Model) *Client { return &Client{cfg: c, http: &http.Client{Timeout: c.Timeout}} }
func (c *Client) Generate(ctx context.Context, msgs []Message, defs []tool.Definition) (Response, error) {
	if c.cfg.APIKey == "" {
		return Response{}, fmt.Errorf("WBOT_MODEL_API_KEY is not configured")
	}
	tools := make([]any, 0, len(defs))
	internalNames := make(map[string]string, len(defs))
	for _, d := range defs {
		modelName := tool.ModelName(d.Name)
		if modelName == "" {
			return Response{}, fmt.Errorf("tool %q has no model-compatible name", d.Name)
		}
		if previous, exists := internalNames[modelName]; exists && previous != d.Name {
			return Response{}, fmt.Errorf("tool names %q and %q both map to %q", previous, d.Name, modelName)
		}
		internalNames[modelName] = d.Name
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": modelName, "description": d.Description, "parameters": d.Parameters}})
	}
	body := map[string]any{"model": c.cfg.Name, "messages": msgs, "max_tokens": c.cfg.MaxOutputTokens}
	if c.cfg.Thinking != "" {
		body["thinking"] = map[string]any{"type": c.cfg.Thinking}
	}
	if c.cfg.ReasoningEffort != "" {
		body["reasoning_effort"] = c.cfg.ReasoningEffort
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	b, _ := json.Marshal(body)
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, e := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if e != nil {
		return Response{}, e
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		resp, e = c.http.Do(req)
		if e == nil && resp.StatusCode != 429 && resp.StatusCode < 500 {
			break
		}
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if attempt < 2 {
			select {
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			case <-ctx.Done():
				return Response{}, ctx.Err()
			}
			req, e = http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
			if e == nil {
				req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
				req.Header.Set("Content-Type", "application/json")
			}
		}
	}
	if e != nil {
		return Response{}, e
	}
	if resp == nil {
		return Response{}, fmt.Errorf("model request failed")
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return Response{}, fmt.Errorf("model API %s: %s", resp.Status, string(rb))
	}
	var raw struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if e = json.Unmarshal(rb, &raw); e != nil {
		return Response{}, e
	}
	if len(raw.Choices) == 0 {
		return Response{}, fmt.Errorf("model returned no choices")
	}
	out := Response{Content: raw.Choices[0].Message.Content, Usage: raw.Usage}
	for _, tc := range raw.Choices[0].Message.ToolCalls {
		var a map[string]any
		if e = json.Unmarshal([]byte(tc.Function.Arguments), &a); e != nil {
			return out, fmt.Errorf("invalid tool arguments: %w", e)
		}
		name := tc.Function.Name
		if internal, ok := internalNames[name]; ok {
			name = internal
		}
		out.ToolCalls = append(out.ToolCalls, domain.ToolCall{ID: tc.ID, Name: name, Arguments: a})
	}
	return out, nil
}
func (c *Client) Consult(ctx context.Context, problem, expected string) (string, error) {
	r, e := c.Generate(ctx, []Message{{Role: "system", Content: "你是只提供分析建议的高级顾问。不要声称执行了任何操作。"}, {Role: "user", Content: problem + "\n期望输出：" + expected}}, nil)
	return r.Content, e
}

func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	r, e := c.Generate(ctx, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, nil)
	return r.Content, e
}

var _ = bufio.NewReader
var _ = time.Second
