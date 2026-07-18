# wbot 执行耗时优化方案

> 日期：2026-07-19  
> 目标：消除任务图中「加载会话历史」和「memory.save」的 LLM 调用瓶颈

## 1. 问题定位

一次典型任务的耗时拆解（来源：`session_fbd5e1da80914bb08b3d7ef9d89845fb` 的最后一次任务）：

```
18:04:14  加载会话历史 ──────── 29s ────────┐
18:04:14  检索长期记忆 <1s                    │ 并行
                                             │
18:04:43  执行目标（排队等历史完成，29s）       │
18:04:48    ├─ 模型调用 #1  4.7s              │
18:04:48    ├─ memory.save  50s ← 元凶        │
18:05:38    └─ 模型调用 #2  3.4s              │
18:06:16  执行目标 complete (总 1m 33s)        │
18:06:16  验收结果 <1s
```

总耗时 **2 分 2 秒**，其中主循环模型推理约 8.2s。两个已定位的辅助 LLM 调用合计约 **79s**，时间线中其余约 35s 仍需通过更细粒度 tracing 解释：

| 瓶颈点 | 耗时 | 调用方 | 使用的模型 |
|--------|------|--------|-----------|
| 加载会话历史 | 29s | `history.summarizeMessages` | advisor (deepseek-v4-pro, reasoning=max) |
| memory.save | 50s | `memory.Upsert` | advisor (deepseek-v4-pro, reasoning=max) |

根本原因：**advisor 模型（deepseek-v4-pro, reasoning=max）被用于两个不需要深度推理的场景**。

## 2. 优化一：历史摘要 LLM 调用

### 2.1 调用链路

```
agent.Run()
  → history.Select(sessionID)
    → prepare()
      → ensureLevelZero()         // 为新消息创建 level-0 段
        → summarizeMessages()     // ← 调 LLM 做摘要
      → mergeSegments()           // 合并同 level 段
        → summarizeSegments()     // ← 调 LLM 做合并
      → renderFrontier()          // 渲染最终摘要文本注入 context
```

### 2.2 问题：输入量过大

**旧代码**（`internal/history/manager.go:271-278`）：

```go
b, _ := json.Marshal(msgs)                    // 完整序列化 domain.Message
out, _ := m.aux.Complete(ctx, system, string(b)) // 发给 LLM
```

`json.Marshal(msgs)` 将整批消息的完整结构体序列化为 JSON。一条 tool 消息的 JSON 可达 4KB+（含命令输出），30 条消息轻松超过 **26KB**。LLM 需要在这堆 JSON 里提取 objectives / decisions / completed_actions 等结构化字段，处理时间远超 30s 超时限制（原超时 30s）。

### 2.3 修复：紧凑文本替代完整 JSON

**新代码**：

```go
out, _ := m.aux.Complete(ctx, system, compactMessagesText(msgs))
```

**`compactMessagesText` 实现**（`internal/history/manager.go:461-494`）：

```go
func compactMessagesText(msgs []domain.Message) string {
    var b strings.Builder
    for _, m := range msgs {
        content := Snip(m.Content, 300)
        switch m.Role {
        case "user":
            fmt.Fprintf(&b, "[user] %s\n", content)
        case "assistant":
            if len(m.ContentJSON) > 0 {
                var calls []domain.ToolCall
                if json.Unmarshal(m.ContentJSON, &calls) == nil {
                    names := make([]string, len(calls))
                    for i, c := range calls {
                        names[i] = c.Name
                    }
                    fmt.Fprintf(&b, "[assistant] %s | calls: %s\n",
                        Snip(m.Content, 200), strings.Join(names, ", "))
                    continue
                }
            }
            fmt.Fprintf(&b, "[assistant] %s\n", content)
        case "tool":
            var result domain.ToolResult
            if json.Unmarshal(m.ContentJSON, &result) == nil {
                fmt.Fprintf(&b, "[tool] %s status=%s %s\n",
                    m.ToolName, result.Status, Snip(result.Summary, 200))
            } else {
                fmt.Fprintf(&b, "[tool] %s %s\n", m.ToolName, content)
            }
        }
    }
    return b.String()
}
```

每条消息压缩为一行紧凑文本，例如：

```
[user] 检查项目中的TODO注释并生成清单
[assistant] 好的，我先查看目录结构 | calls: shell.execute
[tool] shell.execute status=success 命令执行完成
[assistant] 找到了3个TODO，已生成清单文件 | calls: filesystem.write
[tool] filesystem.write status=success 文件已写入
```

实际实现还会保留工具参数和 Artifact ID；模型摘要与确定性摘要采用保守合并，避免压缩过程中静默丢失用户约束、失败、文件变更、Artifact 和未决事项。

### 2.4 修复：段合并同样优化

`summarizeSegments` 中同样替换为 `compactSegmentsText(segs)`，将完整 `HistorySegment` JSON 替换为紧凑格式：

每个 segment 使用紧凑的一行 JSON 表示，但仍携带 `HistorySummary` 的全部字段，而不是只保留 objectives/decisions/completed 等子集。

### 2.5 修复：超时调整

输入量从 26KB 降至 ~2KB 后，模型处理时间大幅缩短。同时将超时从 30s 放宽到 60s，避免偶发网络抖动导致 fallback 到 deterministic 摘要：

```go
auxCtx, cancel := context.WithTimeout(ctx, 60*time.Second) // 原 30s
```

### 2.6 修复：模型切换

三个 `cmd/*/main.go` 中 `agent.New` 的最后一个参数（传给 history 的 aux 模型）从 `advisor` 改为 `mainModel`：

```go
// cmd/wbot-server/main.go
svc := agent.New(s, st, mainModel, tools, mem, mainModel) // 原 advisor
```

### 2.7 效果对比

| | 之前 | 之后 |
|---|---|---|
| 单次输入大小 | ~26KB JSON | ~2KB 纯文本 |
| 使用的模型 | advisor (reasoning=max) | mainModel (fast) |
| 耗时 | 29-47s（常超时 fallback deterministic） | 预计 3-5s |
| 摘要质量 | 超时后 deterministic，丢失细节 | LLM 有充足时间产出结构化摘要 |

## 3. 优化二：memory.save 的 LLM 调用

### 3.1 问题

`memory.save` 工具调用触发 `memory.Upsert()`（`internal/memory/manager.go:631`）：

```go
out, err := m.aux.Complete(ctx,
    `判断候选记忆与已有记忆关系。只输出 {"action":"create|merge|replace|keep_both|mark_conflict|reject","target_id":"..."}。同义事实 merge，新事实取代旧事实 replace，冲突 mark_conflict。`,
    string(b))
```

这是一个**简单文本分类任务**——判断新记忆与已有记忆的关系（新建/合并/替换/冲突）。不需要深度推理。但 `m.aux` 是 advisor（deepseek-v4-pro, reasoning=max），导致单次调用耗时 **50 秒**。

### 3.2 修复

三个 `cmd/*/main.go` 中 `memory.New` 的 generator 参数从 `advisor` 改为 `mainModel`：

```go
// cmd/wbot-server/main.go
mem := memory.New(s.DataRoot+"/memory",
    memory.WithConfig(memory.ConfigFrom(s.Memory)),
    memory.WithGenerator(mainModel),  // 原 advisor
)
```

`memory.Extract`（任务完成后异步提取记忆）也共享此模型，改为 fast 后同样受益。

### 3.3 效果对比

| | 之前 | 之后 |
|---|---|---|
| 使用的模型 | advisor (reasoning=max) | mainModel (fast) |
| memory.save 耗时 | 50s | 预计 3-5s |
| 任务总耗时 | 2m 2s | 预计 ~20s |

## 4. 模型职责划分

优化后的模型分工：

```
mainModel (deepseek-v4-flash, fast, 120s timeout)
  ├─ agent 主循环 (executor)
  ├─ 历史摘要 (history summarization)
  ├─ 记忆去重/提取 (memory Upsert/Extract)
  └─ 辅助摘要与记忆任务

advisor (deepseek-v4-pro, reasoning=max, 180s timeout)
  └─ consult_advisor 工具（用户显式请求高能力分析时）
```

## 5. 工具调用超时

新增全局工具调用硬超时 120s（`internal/agent/service.go:383-408`），防止工具挂死导致任务永久阻塞：

```go
func (s *Service) executeTool(ctx context.Context, task domain.Task, call domain.ToolCall) toolExecution {
    toolCtx, toolCancel := context.WithTimeout(ctx, 120*time.Second)
    defer toolCancel()

    done := make(chan toolOut, 1)
    go func() {
        r, a := s.tools.Execute(toolCtx, task.ID, call)
        done <- toolOut{result: r, approval: a}
    }()

    select {
    case out := <-done:
        result, approval = out.result, out.approval
    case <-toolCtx.Done():
        // 超时返回 TOOL_TIMEOUT 错误，模型可决定重试或放弃
        result = domain.ToolResult{Status: "error", Summary: "工具调用超时（2m0s）", ...}
    }
}
```

## 6. 涉及文件清单

| 文件 | 变更类型 | 说明 |
|------|---------|------|
| `internal/history/manager.go` | 修改 | `compactMessagesText` / `compactSegmentsText` 替代完整 JSON；超时 30s → 60s |
| `internal/memory/manager.go` | 无改动 | 通过 `WithGenerator` 注入的模型变更 |
| `internal/agent/service.go` | 修改 | 全局工具 120s 硬超时 |
| `cmd/wbot-server/main.go` | 修改 | history 和 memory 改用 mainModel |
| `cmd/wbot-desktop/main.go` | 修改 | 同上 |
| `cmd/wbot-wails/main.go` | 修改 | 同上，同时修复了缺失 `mainModel` 变量的问题 |

## 7. 预期效果与验证要求

重启服务后，以之前耗时 2m 2s 的任务为例：

```
加载会话历史  29s → 3-5s    (compact text + fast model)
检索长期记忆  <1s → <1s     (不变)
执行目标      93s → ~12s     (8s 模型 + memory.save 3-5s)
验收结果      <1s → <1s     (不变)
─────────────────────────
总耗时       122s → ~20s     (减少 83%)
```

以上是目标值，不是本次 commit 已完成的基准结论。上线结论应来自同一任务、同一模型配置的多次冷/热运行 p50/p95；复杂模板额外执行的 research/plan 模型节点也必须单独计入。
