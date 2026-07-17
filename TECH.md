# wbot 自主 Agent 技术设计文档

> 文档版本：0.4  
> 状态：V1 已实现并通过自动化验收  
> 日期：2026-07-18

## 1. 背景

wbot 的目标是构建一个能够持续理解目标、规划工作、调用工具、管理上下文并在授权范围内自主执行的数字工作者。

首个版本需要支持以下核心能力：

1. 使用个性化配置文件构建 System Prompt。
2. 支持高、低成本模型；默认由低成本模型执行，高成本模型以工具形式按需调用。
3. 支持跨会话的长期记忆。
4. 支持消息历史和上下文窗口管理。
5. 使用任务图表达目标、步骤和依赖关系。
6. 通过环境变量设置权限等级，支持审批式和完全访问式运行。

## 2. 设计目标

### 2.1 功能目标

- Agent 可以根据配置文件形成稳定、可版本化的身份和工作方式。
- Agent 默认使用低成本模型完成规划、推理和工具调用。
- 低成本模型可以将复杂问题提交给高成本模型咨询。
- Agent 可以从历史任务中检索有效记忆，并沉淀新的候选记忆。
- 长对话和大型工具结果不会无限占用上下文窗口。
- Agent 可以创建、更新、执行和恢复持久化任务图。
- 所有产生副作用的操作均经过权限引擎判断。
- Agent 中断后可以从持久化检查点恢复，而不是依赖模型回忆。
- 每次模型调用、工具调用、审批和状态变化均可审计。

### 2.2 非目标

首个版本暂不包含：

- 多个自治 Agent 之间的开放式社会协作。
- 无约束地自动修改自身代码或安全策略。
- 基于执行日志自动训练或微调基础模型。
- 复杂的分布式任务调度和跨机器容灾。
- 绕过操作系统或外部服务自身的权限控制。

## 3. 设计原则

### 3.1 自主执行不等于自主授权

Agent 可以自主决定如何完成已授权目标，但不能自行扩大目标、资源范围或操作权限。

### 3.2 状态与上下文分离

- 任务图保存工作状态。
- 消息历史保存本次交互过程。
- 长期记忆保存跨任务仍然有价值的信息。
- Artifact Store 保存完整工具输出和交付物。

模型上下文只是上述状态的临时视图，不是事实来源。

### 3.3 低成本优先，高成本按需使用

低成本模型是唯一默认执行入口。高成本模型作为 Advisor 工具返回建议，默认不直接执行有副作用的工具。

### 3.4 可恢复、可验证、可审计

任务执行需要支持检查点、幂等调用、结果验收和完整事件记录。工具返回成功不等于任务完成，只有验收标准通过后才能关闭任务。

### 3.5 稳定前缀优先

System Prompt 中稳定内容放在前部，动态任务、历史和记忆放在后部，以提高 Prompt Cache 命中率。

## 4. 总体架构

```text
┌─────────────────────────────────────────────────────────────┐
│                        客户端层                             │
│ Browser Web UI / Wails Desktop / Message Channel / API     │
└─────────────────────────────┬───────────────────────────────┘
                              │
┌─────────────────────────────▼───────────────────────────────┐
│                       服务接入层                            │
│ HTTP API / SSE / Wails Adapter / Authentication            │
└─────────────────────────────┬───────────────────────────────┘
                              │
┌─────────────────────────────▼───────────────────────────────┐
│                     Agent Runtime                           │
│  Loop / Context Builder / Checkpoint / Event Dispatcher    │
└──────┬───────────────┬──────────────┬───────────────┬───────┘
       │               │              │               │
┌──────▼──────┐ ┌──────▼──────┐ ┌─────▼──────┐ ┌──────▼──────┐
│ Model Router│ │ Task Graph  │ │  Memory    │ │  History    │
│ Cheap Model │ │ Scheduler   │ │  Manager   │ │  Manager    │
│ Advisor Tool│ │ Evaluator   │ │ Retrieval  │ │ Compaction  │
└──────┬──────┘ └─────────────┘ └────────────┘ └─────────────┘
       │
┌──────▼──────────────────────────────────────────────────────┐
│                      Tool Gateway                           │
│        Registry → Permission Engine → Tool Executor        │
└─────────────────────────────┬───────────────────────────────┘
                              │
┌─────────────────────────────▼───────────────────────────────┐
│                       存储与观测                            │
│ Task DB / Message DB / Memory DB / Artifact / Event Log    │
└─────────────────────────────────────────────────────────────┘
```

## 5. 技术栈与推荐项目结构

### 5.1 技术栈

| 领域 | V1 选择 |
|---|---|
| 服务端语言 | Go |
| 并发模型 | Goroutine、Channel、`context.Context` |
| HTTP 服务 | Go `net/http` |
| 流式事件 | SSE，后续按需补充 WebSocket |
| 模型协议 | OpenAI 兼容协议 |
| 默认模型 | `deepseek-v4-flash` |
| Advisor 模型 | `deepseek-v4-pro` |
| 数据库 | SQLite，本地与云端统一使用 |
| Artifact 与记忆 | 本地文件系统，本地与云端统一使用 |
| Web UI | React + TypeScript + Vite |
| 桌面客户端 | Wails v2，复用 Web UI |

Go 不需要额外的异步运行框架。Agent Runtime 使用 Goroutine 执行任务节点，使用 `context.Context` 传递取消、超时和 Trace 信息，使用有界 Worker Pool 控制并发。

### 5.2 Go 项目结构

```text
wbot/
├── cmd/
│   ├── wbot-server/
│   │   └── main.go
│   └── wbot-desktop/
│       └── main.go
├── internal/
│   ├── agent/
│   │   ├── runtime.go
│   │   ├── loop.go
│   │   ├── context.go
│   │   └── checkpoint.go
│   ├── config/
│   │   ├── settings.go
│   │   └── schema.go
│   ├── model/
│   │   ├── protocol.go
│   │   ├── openai.go
│   │   ├── router.go
│   │   └── advisor.go
│   ├── memory/
│   │   ├── manager.go
│   │   ├── extractor.go
│   │   ├── retrieval.go
│   │   └── consolidation.go
│   ├── history/
│   │   ├── manager.go
│   │   ├── compactor.go
│   │   └── token_budget.go
│   ├── task/
│   │   ├── graph.go
│   │   ├── planner.go
│   │   ├── scheduler.go
│   │   └── evaluator.go
│   ├── permission/
│   │   ├── engine.go
│   │   ├── policy.go
│   │   └── approval.go
│   ├── tool/
│   │   ├── registry.go
│   │   ├── executor.go
│   │   ├── filesystem.go
│   │   ├── shell.go
│   │   └── advisor.go
│   ├── transport/
│   │   ├── httpapi/
│   │   ├── message/
│   │   └── wails/
│   └── storage/
│       ├── database.go
│       ├── repository.go
│       ├── artifact.go
│       └── event_log.go
├── profiles/
│   └── default.yaml
├── prompts/
│   ├── base.md
│   └── fragments/
├── web/
│   ├── src/
│   └── package.json
├── migrations/
├── test/
└── go.mod
```

模块之间通过明确的协议交互。业务模块不能直接访问其他模块的数据库表，应通过 Repository 或 Service 接口访问。

## 6. 配置系统与个性化 System Prompt

### 6.1 配置来源

配置按以下优先级合并，后者覆盖前者：

```text
内置默认值
→ 项目配置文件
→ 用户 Profile
→ 环境变量
→ 启动参数
```

环境变量只保存部署和安全相关配置。人格、工作习惯等内容保存在 Profile 文件中，避免将复杂 Prompt 塞入环境变量。

### 6.2 Profile 格式

推荐使用 YAML：

```yaml
version: 1

identity:
  name: "wbot"
  role: "自主数字员工"
  language: "zh-CN"

personality:
  tone: "简洁、直接、专业"
  initiative: "high"

working_style:
  plan_before_execution: true
  verify_before_complete: true
  ask_when_ambiguous: true
  report_format: "summary_first"

memory:
  enabled: true
  store_user_preferences: true
  store_project_decisions: true

constraints:
  never_store_secrets: true
  never_claim_success_without_evidence: true

custom_instructions: |
  优先推进不依赖用户决策的工作。
  完成任务时给出结果和验证证据。
```

### 6.3 Prompt 组装顺序

```text
1. Agent 基础身份和核心约束          稳定
2. 工具调用与任务图协议              稳定
3. 用户 Profile                     低频变化
4. 工作区与权限信息                  中频变化
5. 当前任务图摘要                    高频变化
6. 检索到的长期记忆                  高频变化
7. 消息历史                          高频变化
```

Prompt Builder 输出以下元数据：

```json
{
  "profile_version": 1,
  "profile_hash": "sha256:...",
  "prompt_template_version": "0.1",
  "estimated_tokens": 12450
}
```

每次模型调用都记录这些版本信息，保证后续可以重放和定位行为变化。

### 6.4 配置校验

- 启动时校验 Profile Schema。
- 不认识的字段默认报错，防止拼写错误被静默忽略。
- 配置热更新只影响下一次模型调用，不修改已经发送的历史消息。
- Profile 中不得直接定义绕过权限引擎的规则。

## 7. 模型路由与 Advisor 工具

### 7.1 角色划分

| 角色 | 默认职责 | 是否执行副作用工具 |
|---|---|---|
| 低成本模型 | 规划、选择工具、维护任务、生成回复 | 是，经过权限引擎 |
| 高成本 Advisor | 复杂推理、复核方案、分析失败 | 默认否 |

所有用户请求首先进入低成本模型。高成本模型不能成为隐式默认模型。

### 7.2 模型配置

```yaml
models:
  default:
    provider: "deepseek"
    protocol: "openai"
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-flash"
    thinking: "disabled"
    max_output_tokens: 16000
    timeout_seconds: 120

  advisor:
    provider: "deepseek"
    protocol: "openai"
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-pro"
    thinking: "enabled"
    reasoning_effort: "max"
    max_output_tokens: 32000
    timeout_seconds: 180

advisor:
  enabled: true
  max_calls_per_task: 3
  max_calls_per_step: 1
  max_context_tokens: 60000
```

API Key 仅从通用的 `WBOT_MODEL_API_KEY` 环境变量或操作系统安全凭据存储读取，不写入 YAML、Profile、消息历史或事件日志。Advisor 模型通过 `WBOT_ADVISOR_MODEL` 独立配置，服务地址可通过 `WBOT_ADVISOR_BASE_URL` 覆盖。启动时可以调用 OpenAI 兼容的 `GET /models` 检查模型是否可用，但模型名称仍以本地配置为准，避免运行中静默切换模型。

内部模型协议不能直接暴露某个供应商的 SDK 类型：

```go
type ModelClient interface {
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
	Stream(ctx context.Context, req GenerateRequest) (<-chan StreamEvent, error)
}
```

OpenAI Adapter 负责 Chat Completions 请求、流式事件、Tool Call 和供应商扩展字段之间的转换。DeepSeek 的 `thinking`、`reasoning_effort` 等参数通过受控扩展字段传递，业务代码只读取统一的 `GenerateResponse`。多轮工具调用需要由 Adapter 正确保留供应商要求回放的推理相关字段，不能在通用消息压缩中误删。

### 7.3 Advisor 工具协议

```json
{
  "name": "consult_advisor",
  "description": "在复杂、高风险、连续失败或需要高质量复核时咨询高能力模型。返回建议，不直接执行操作。",
  "input_schema": {
    "type": "object",
    "required": ["problem", "expected_output"],
    "properties": {
      "problem": {"type": "string"},
      "relevant_context": {"type": "string"},
      "attempts": {
        "type": "array",
        "items": {"type": "string"}
      },
      "expected_output": {"type": "string"}
    }
  }
}
```

Advisor 返回结构：

```json
{
  "analysis_summary": "...",
  "recommendation": "...",
  "risks": ["..."],
  "suggested_next_steps": ["..."],
  "confidence": 0.82
}
```

### 7.4 调用限制

- 每个任务和任务节点分别设置调用上限。
- Advisor 不允许递归调用自己。
- 只发送解决问题所需的最小上下文，避免复制全部消息历史。
- 敏感信息在发送前执行脱敏或范围校验。
- Advisor 建议必须回到低成本模型和任务图，不直接视为执行结果。
- 记录模型、Token、耗时、费用、调用原因和后续是否采纳。

### 7.5 可选的运行时强制咨询

除模型主动选择外，以下情况可以由运行时要求低成本模型咨询 Advisor：

- 同一节点连续失败达到阈值。
- 高风险操作缺少明确执行方案。
- Evaluator 连续判定验收失败。
- Planner 无法生成可执行节点。

该能力应可配置关闭，以保持“由模型选择”的默认语义。

## 8. 记忆系统

### 8.1 记忆分类

| 类型 | 内容 | 默认作用域 | 生命周期 |
|---|---|---|---|
| user | 用户偏好和稳定习惯 | 用户 | 长期 |
| project | 项目事实、架构和决策 | 项目 | 长期 |
| episodic | 历史任务过程和结果 | 任务/项目 | 可衰减 |
| procedural | Skill、成功工作流、失败经验 | 用户/项目 | 长期 |

### 8.2 文件组织

V1 不默认依赖向量数据库。记忆采用“索引文件 + 分类记忆文件”的可读文件结构：

```text
.wbot/
└── memory/
    ├── index.yaml
    ├── user/
    │   ├── preferences.md
    │   └── communication.md
    ├── project/
    │   ├── architecture.md
    │   ├── conventions.md
    │   └── decisions.md
    ├── episodic/
    │   └── 2026-07.md
    └── procedural/
        ├── workflows.md
        └── failures.md
```

`index.yaml` 只保存能够帮助模型选择记忆的信息，不复制完整内容：

```yaml
version: 1
updated_at: "2026-07-17T10:00:00+08:00"
entries:
  - id: "project-architecture"
    type: "project"
    path: "project/architecture.md"
    summary: "wbot 的模块边界、运行时和存储架构"
    topics: ["architecture", "runtime", "storage"]
    priority: 0.9
    updated_at: "2026-07-17T10:00:00+08:00"
```

记忆加载采用两阶段流程：

```text
当前任务 + 最近消息
→ 加载 memory/index.yaml
→ 低成本模型选择相关记忆 ID
→ 运行时校验路径和读取预算
→ 读取选中的分类记忆文件
→ 将内容作为独立 Memory Context Block 注入 Prompt
```

索引选择输出必须结构化：

```json
{
  "selected_memory_ids": ["project-architecture"],
  "reason": "当前任务涉及 Agent Runtime 设计"
}
```

运行时负责去重、路径校验、单文件大小限制和总 Token 预算。模型不能通过索引读取 `.wbot/memory` 之外的任意路径。

### 8.3 逻辑数据模型

```json
{
  "id": "mem_01...",
  "type": "project",
  "scope": {
    "user_id": "user_01",
    "project_id": "project_01"
  },
  "content": "项目服务端统一使用 Go。",
  "summary": "服务端语言为 Go",
  "tags": ["golang", "environment"],
  "source": {
    "task_id": "task_01",
    "message_id": "msg_01",
    "event_id": "event_01"
  },
  "confidence": 0.95,
  "importance": 0.80,
  "created_at": "2026-07-17T10:00:00+08:00",
  "last_accessed_at": null,
  "expires_at": null,
  "status": "active",
  "version": 1
}
```

### 8.4 写入流程

```text
任务结束或形成稳定决策
→ 提取候选记忆
→ 检测密钥和敏感信息
→ 判断事实/推测及可信度
→ 与现有记忆检索比对
→ 新增、合并、替换或丢弃
→ 更新分类文件和 index.yaml
```

以下内容默认不写入长期记忆：

- 密码、Token、私钥和会话凭证。
- 未经验证的模型推测。
- 可从项目文件随时读取的冗长原文。
- 临时错误堆栈和一次性工具输出。
- 与未来任务无关的寒暄。

### 8.5 检索流程

V1 由低成本模型根据索引中的类型、摘要、主题、优先级和更新时间选择相关记忆：

```text
候选 = 作用域过滤(index)
    → 模型语义选择(memory IDs)
    → 优先级和读取预算裁剪
    → 读取分类文件
```

索引规模过大时，可以先使用关键词和类型过滤缩小候选集合。向量检索作为未来可选的召回插件，不是 V1 的默认依赖。返回结果必须包含记忆 ID 和来源，便于模型引用以及运行时追踪错误记忆。

### 8.6 修正与遗忘

- 记忆采用软删除，保留修改历史。
- 新事实与旧事实冲突时不直接拼接，由 Consolidator 判断替换、并存或请求确认。
- 用户可以查看、编辑和删除自己的记忆。
- 过期的情景记忆降低检索权重，达到期限后进入归档。

## 9. 消息历史与上下文管理

### 9.1 数据边界

```text
Message History：用户和 Agent 在本次会话中说了什么
Task Graph：当前工作做到哪里
Memory：未来任务仍值得复用的信息
Artifact：无法安全放进上下文的完整内容
```

不得使用对话摘要代替任务图，也不得把全部消息历史直接写入长期记忆。

### 9.2 消息模型

```json
{
  "id": "msg_01...",
  "session_id": "session_01",
  "task_id": "task_01",
  "role": "user",
  "content": [],
  "created_at": "...",
  "token_count": 123,
  "compaction_state": "raw",
  "parent_message_id": null
}
```

### 9.3 上下文预算

默认预算可按比例分配：

| 内容 | 预算比例 |
|---|---:|
| System Prompt 与工具协议 | 20% |
| 当前任务图 | 15% |
| 检索记忆 | 15% |
| 最近消息 | 25% |
| 工具结果 | 20% |
| 输出保留 | 5% |

比例是上限指导，不要求每次填满。Context Builder 应以 Token 计数器的实际结果为准。

### 9.4 压缩层级

1. **Micro compaction**：截断重复日志、无关字段和超长单行。
2. **Tool snapshot**：把完整工具输出转换为结构化摘要，原文存入 Artifact Store。
3. **History summary**：摘要较早消息，保留目标、决策、未完成事项和引用。
4. **Reactive compaction**：模型服务仍报告超长时，紧急压缩早期上下文并重试一次。

工具快照格式：

```json
{
  "tool_call_id": "call_01",
  "tool": "shell.execute",
  "status": "success",
  "request_summary": "运行单元测试",
  "result_summary": "128 项通过，2 项失败",
  "important_details": [
    "test_login_expired failed",
    "test_retry_limit failed"
  ],
  "artifact_ref": "artifact://tool-result/call_01"
}
```

### 9.5 压缩约束

- 最近若干轮消息保留原文。
- 用户约束、任务验收标准、审批结果不得仅存在于自由文本摘要中。
- 摘要必须记录覆盖的消息 ID 范围。
- 同一段历史不得在每轮重复摘要。
- 压缩不能修改持久化任务状态。

## 10. 任务图

### 10.1 图模型

任务使用有向无环图表达。节点是可执行工作单元，边表示依赖关系。

```json
{
  "id": "task_01",
  "objective": "生成并发送项目周报",
  "acceptance_criteria": [
    "包含进展、问题和下周计划",
    "关键数据经过验证",
    "发送操作获得授权"
  ],
  "status": "running",
  "nodes": [
    {
      "id": "collect",
      "title": "收集项目数据",
      "depends_on": [],
      "status": "completed"
    },
    {
      "id": "draft",
      "title": "生成周报草稿",
      "depends_on": ["collect"],
      "status": "running"
    },
    {
      "id": "verify",
      "title": "核验内容",
      "depends_on": ["draft"],
      "status": "pending"
    },
    {
      "id": "send",
      "title": "发送周报",
      "depends_on": ["verify"],
      "status": "pending"
    }
  ]
}
```

### 10.2 节点状态

```text
PENDING
  → READY
  → RUNNING
  → VERIFYING
  → COMPLETED

RUNNING
  → WAITING_APPROVAL
  → WAITING_EXTERNAL
  → FAILED
  → CANCELLED
```

`FAILED` 节点可以经过重试或重新规划回到 `READY`。任何状态变化必须由确定性运行时代码校验，模型只能提出变更请求。

### 10.3 节点模型

```json
{
  "id": "node_01",
  "task_id": "task_01",
  "title": "核验周报数据",
  "description": "将草稿中的指标与原始数据对比",
  "depends_on": ["node_00"],
  "status": "ready",
  "inputs": [],
  "expected_outputs": [],
  "acceptance_criteria": [],
  "risk_level": "low",
  "retry": {
    "attempt": 0,
    "max_attempts": 2
  },
  "timeout_seconds": 600,
  "assigned_agent": "main",
  "result": null,
  "evidence": []
}
```

### 10.4 调度规则

- 只有全部依赖节点完成后，节点才可以从 `PENDING` 进入 `READY`。
- Scheduler 只选择 `READY` 节点。
- V1 允许无冲突的 `READY` 节点并行执行。
- 对同一外部资源存在写冲突的节点不得并行。
- Scheduler 使用有界 Worker Pool，默认并行度由 `WBOT_TASK_MAX_PARALLELISM` 控制。
- 并发节点分别使用独立消息分支和工具结果，完成后由运行时确定性合并任务状态。
- 节点开始执行前获取资源锁；至少支持工作区路径、外部服务对象和用户声明资源三类锁。
- 任一节点失败不会自动取消无依赖关系的节点；是否取消由任务失败策略决定。
- 每轮模型调用前生成任务图摘要，而不是注入完整历史图变更。
- Todo 是任务图的视图，不维护独立的 Todo 状态。

### 10.5 验收

每个节点和顶层任务都必须具有完成标准。Evaluator 输出：

```json
{
  "passed": false,
  "criteria": [
    {
      "criterion": "关键数据经过验证",
      "passed": false,
      "reason": "缺少数据源引用"
    }
  ],
  "recommended_action": "replan"
}
```

只有顶层验收标准全部通过，任务才能进入 `COMPLETED`。

## 11. 权限与审批

### 11.1 环境变量

```bash
WBOT_PERMISSION_MODE=approval
WBOT_WORKSPACE_ROOT=/absolute/path/to/workspace
WBOT_ALLOW_SHELL=true
WBOT_ALLOW_NETWORK=true
WBOT_ALLOW_EXTERNAL_WRITE=false
WBOT_TASK_MAX_PARALLELISM=4
```

`WBOT_PERMISSION_MODE` 支持：

- `approval`：高风险或策略指定操作等待审批。
- `full_access`：在配置作用域内无需人工审批。

未知值必须导致启动失败，不得自动降级或升级权限。

### 11.2 权限判断顺序

```text
系统不可覆盖的安全限制
→ 环境权限模式
→ 工具级策略
→ 文件、网络和资源作用域
→ 当前任务授权
→ 风险等级
→ ALLOW / ASK / DENY
```

### 11.3 风险级别

| 级别 | 示例 | approval 默认行为 |
|---|---|---|
| L0 | 读取文件、查询状态、计算 | ALLOW |
| L1 | 工作区内创建或修改文件 | ALLOW，可配置为 ASK |
| L2 | 执行 Shell、联网写入、发送消息、部署 | ASK |
| L3 | 删除、资金、生产变更、不可逆操作 | ASK 或 DENY |

工具必须在注册时声明风险信息，不能完全依赖模型描述：

```json
{
  "name": "email.send",
  "risk_level": "L2",
  "side_effect": true,
  "idempotent": false,
  "approval_scope": "exact_arguments"
}
```

V1 明确规定：`approval` 模式下，授权工作区内的普通创建和写入操作默认为 `ALLOW`。越出工作区、覆盖敏感文件、批量改写或删除操作不属于普通写入，仍按 L2/L3 规则处理。

### 11.4 审批模型

```json
{
  "id": "approval_01",
  "task_id": "task_01",
  "node_id": "send",
  "tool_name": "email.send",
  "arguments_digest": "sha256:...",
  "arguments_preview": {
    "recipient": "customer@example.com",
    "subject": "项目周报"
  },
  "risk_level": "L2",
  "reason": "向外部联系人发送周报",
  "status": "pending",
  "created_at": "...",
  "expires_at": "..."
}
```

审批只对参数摘要完全一致的调用有效。收件人、路径、命令或正文等关键参数变化后必须重新审批。

### 11.5 完全访问模式

`full_access` 仅跳过人工审批，仍然保留：

- 工具参数 Schema 校验。
- 工作区和网络作用域限制。
- 操作系统及外部服务权限。
- 幂等保护和重复调用检测。
- 事件日志、费用统计和结果验证。
- 系统级禁止规则。

### 11.6 工具执行协议

```go
decision := permissionEngine.Evaluate(ctx, toolCall, task)

switch decision.Kind {
case permission.Deny:
	return tool.Result{}, tool.NewError("PERMISSION_DENIED", decision.Reason)
case permission.Ask:
	approval, err := approvalStore.Create(ctx, task, toolCall)
	if err != nil {
		return tool.Result{}, err
	}
	taskGraph.MarkWaitingApproval(approval.ID)
	return tool.Result{Status: tool.WaitingApproval}, nil
default:
	return toolExecutor.Execute(ctx, toolCall, buildIdempotencyKey(task, toolCall))
}
```

## 12. Agent 主循环

```go
func (r *Runtime) RunTask(ctx context.Context, taskID string) (TaskResult, error) {
	for {
		task, err := r.tasks.Load(ctx, taskID)
		if err != nil {
			return TaskResult{}, err
		}
		if task.IsTerminal() {
			return task.Result, nil
		}

		ready := task.Graph.ReadyNodes()
		if len(ready) > 1 {
			// Worker Pool 只并行执行资源锁不冲突的节点。
			if err := r.scheduler.RunReadyNodes(ctx, task, ready); err != nil {
				return TaskResult{}, err
			}
		}

		if err := r.checkpoints.Save(ctx, task); err != nil {
			return TaskResult{}, err
		}

		agentContext, err := r.contextBuilder.Build(ctx, ContextInput{
			Profile:     r.profiles.Current(),
			Task:        task,
			Messages:    r.history.Select(task),
			Memories:    r.memory.Retrieve(ctx, task),
			Permissions: r.permissions.Describe(),
		})
		if err != nil {
			return TaskResult{}, err
		}

		response, err := r.models.Default().Generate(ctx, agentContext, r.tools.Schemas())
		if err != nil {
			return r.handleModelError(ctx, task, err)
		}
		r.events.RecordModelResponse(ctx, task, response)

		if len(response.ToolCalls) > 0 {
			if err := r.handleToolCalls(ctx, task, response.ToolCalls); err != nil {
				return TaskResult{}, err
			}
			r.history.CompactIfNeeded(ctx, task)
			r.checkpoints.Save(ctx, task)
			continue
		}

		verification := r.evaluator.Verify(ctx, task, response)
		if verification.Passed {
			r.memory.ExtractAndConsolidate(ctx, task)
			r.tasks.Complete(ctx, task, response, verification)
			return TaskResult{Status: Completed, Response: response}, nil
		}

		r.planner.Replan(ctx, task, verification)
		r.history.AppendVerificationFailure(ctx, task, verification)
		r.checkpoints.Save(ctx, task)
	}
}
```

## 13. 工具系统

### 13.1 Tool Registry

Registry 保存：

- 工具名称和描述。
- 输入、输出 Schema。
- 风险级别和副作用属性。
- 超时与最大输出大小。
- 是否幂等。
- 权限作用域。
- 实际执行 Handler。

模型只能看到 Registry 导出的公开 Schema，不能获得内部凭证和执行对象。

### 13.2 统一结果

```json
{
  "tool_call_id": "call_01",
  "status": "success",
  "data": {},
  "summary": "文件已更新",
  "artifacts": [],
  "retryable": false,
  "error": null,
  "metrics": {
    "duration_ms": 128
  }
}
```

错误结构：

```json
{
  "code": "RATE_LIMITED",
  "message": "服务暂时限流",
  "retryable": true,
  "retry_after_ms": 2000,
  "details": {}
}
```

结构化错误既供运行时处理，也作为模型下一轮推理输入。

## 14. HTTP API 与交互入口

### 14.1 HTTP API

V1 提供 HTTP API，核心资源包括：

```text
POST   /api/v1/workspaces/open
GET    /api/v1/workspaces
POST   /api/v1/sessions
GET    /api/v1/sessions/{session_id}
POST   /api/v1/sessions/{session_id}/messages
GET    /api/v1/sessions/{session_id}/events
GET    /api/v1/tasks/{task_id}
POST   /api/v1/tasks/{task_id}/cancel
GET    /api/v1/approvals
POST   /api/v1/approvals/{approval_id}/approve
POST   /api/v1/approvals/{approval_id}/reject
GET    /api/v1/artifacts/{artifact_id}
```

消息提交使用普通 HTTP 请求。模型增量输出、工具进度、任务图变化和审批事件通过 SSE 推送。SSE 断线重连时使用事件 ID 从 Event Log 续传，避免 UI 丢失状态。

### 14.2 审批入口

审批由统一 Approval Service 管理，并通过适配器暴露给：

- Web UI：显示待审批列表、参数预览、风险说明和批准/拒绝按钮。
- 消息渠道：发送审批卡片或带签名的一次性审批链接。

不同入口不能自行修改任务状态，只能调用 Approval Service。审批结果写入 Event Log 后唤醒等待中的任务节点。

消息渠道适配器至少需要实现：

```go
type ApprovalChannel interface {
	Notify(ctx context.Context, approval Approval) error
	ValidateCallback(ctx context.Context, payload []byte, signature string) (Decision, error)
}
```

### 14.3 Workspace 与 Session

Session 必须属于一个 Workspace，Agent 的文件工具默认只能访问该 Workspace 的授权资源。Workspace 根据部署位置分为：

| 类型 | 资源位置 | 典型使用方式 |
|---|---|---|
| `local` | 客户端本机目录 | Wails 内置 Agent 或 Local Runner |
| `server` | 云端 Agent 所在机器或挂载卷 | 浏览器或远程桌面客户端 |
| `git` | 云端按任务克隆的仓库 | 云端编码和自动化任务 |
| `companion` | 用户本机目录，由 Companion 暴露 | 云端调度、本地执行 |

本地和服务端 Workspace 使用规范化绝对路径；远程客户端只看到 Workspace ID、显示名称和相对路径，不获得无意义的服务端绝对路径。

```text
Workspace
└── Session A
    ├── Message History
    ├── Task Graph
    ├── Approvals
    └── Artifacts
```

本地模式的会话数据库默认保存在 wbot 应用数据目录；云端模式的会话状态保存在服务端数据库。两种模式都不应污染用户项目。项目内只放用户明确选择纳入版本管理的 `.wbot/profile.yaml`、记忆文件和 Skill。

### 14.4 Web UI 与桌面客户端

推荐使用 React + TypeScript 构建同一套界面，并通过 `AgentClient` 接口隔离通信方式：

```ts
interface AgentClient {
  openWorkspace(path?: string): Promise<Workspace>;
  createSession(workspaceId: string): Promise<Session>;
  sendMessage(sessionId: string, content: string): Promise<void>;
  subscribeEvents(sessionId: string): AsyncIterable<AgentEvent>;
  decideApproval(id: string, decision: "approve" | "reject"): Promise<void>;
}
```

- 浏览器版本使用 HTTP + SSE Adapter。
- Wails 桌面版本在本地模式下使用 Go/JavaScript Binding 和 Wails Event Adapter。
- Wails 桌面版本在云端模式下使用与浏览器相同的 HTTPS + SSE Adapter。
- 两个 Adapter 调用同一个 Go Application Service，不能分别实现业务逻辑。

桌面端通过系统文件夹选择器打开本地 Workspace。纯 Web 版本不能直接任意读取浏览器所在电脑的文件夹，只能选择服务端允许的目录；如果需要操作用户本机文件，应使用桌面客户端或本地 Companion Service。

建议的界面布局：

```text
┌──────────────┬───────────────────────────┬──────────────────┐
│ Workspace    │ Conversation              │ Task / Context   │
│ Session List │ Messages                  │ Task Graph       │
│ Files        │ Tool Calls                │ Approvals        │
│              │ Composer                  │ Changed Files    │
└──────────────┴───────────────────────────┴──────────────────┘
```

同一个 Wails 客户端提供连接模式选择：

```text
Local
  Agent Core、SQLite 和 Workspace 均位于本机

Cloud
  客户端配置 Server URL，通过 HTTPS/SSE 连接云端 Agent Service
```

前端组件不感知 Agent 位于本机还是云端，只依赖 `AgentClient`。V1 先稳定 HTTP/SSE 协议和 Web UI，再用 Wails v2 封装桌面客户端；桌面客户端同时支持 Local 和 Cloud 两种连接模式。

### 14.5 部署模式

#### 模式 A：本地一体化

```text
Wails Desktop
├── React UI
├── Go Agent Core
├── SQLite
└── Local Workspace
```

适合个人用户处理本地项目。应用不需要开放网络端口，文件访问和审批均在本机完成。

#### 模式 B：云端服务

```text
Browser / Wails Desktop
        │ HTTPS + SSE
        ▼
Cloud Agent Service
├── Agent Runtime Workers
├── SQLite
├── Local Artifact/Memory Files
└── Server/Git Workspace
```

适合长时间任务、多设备访问和集中管理。云端进程的数据目录必须位于持久化磁盘或持久卷中，不能依赖容器临时文件系统。Agent 只能操作云端可见的仓库、挂载卷或本地文件，不能直接读取客户端本机文件夹。

#### 模式 C：云端调度 + 本地执行

```text
Local UI ───────► Cloud Agent Service
                       │ 签名任务
                       ▼
                Local Companion
                       │
                       ▼
                Local Workspace
```

当云端 Agent 需要操作用户本机文件时，必须安装 Local Companion。Companion 主动向云端建立加密长连接，不要求用户开放入站端口；它在本地再次执行 Permission Engine、Workspace 范围检查和审批策略。云端签名只能表达请求，不能绕过本地策略。

V1 可以先实现模式 A 和模式 B。模式 C 涉及设备注册、双向认证、断线恢复和远程工具协议，建议在核心任务协议稳定后实现。

## 15. 存储设计

本地一体化和云端部署统一使用 SQLite 与本地文件系统。数据库保存结构化状态和审计元数据；记忆、Artifact、Workspace 与大型工具结果保存在数据根目录中。

推荐数据目录：

```text
WBOT_DATA_ROOT/
├── wbot.db
├── memory/
│   ├── users/
│   └── workspaces/
├── artifacts/
│   └── sha256-prefix/
├── workspaces/
│   ├── server/
│   └── git/
├── logs/
└── backups/
```

云端使用时通过环境变量指定持久化目录：

```bash
WBOT_DATA_ROOT=/var/lib/wbot
WBOT_DATABASE_PATH=/var/lib/wbot/wbot.db
```

容器部署需要把 `/var/lib/wbot` 整体挂载到持久卷。SQLite 文件、WAL 文件和关联数据目录应位于同一持久化边界内。

核心表：

```text
profiles
sessions
messages
message_summaries
tasks
task_nodes
task_edges
task_checkpoints
memories
memory_versions
tool_calls
approvals
artifacts
events
model_usage
```

### 15.1 一致性要求

- 工具结果、任务节点变化和事件日志尽可能在同一事务中提交。
- 外部工具执行无法纳入数据库事务，必须使用幂等键和调用状态处理不确定结果。
- 进程崩溃后，状态为 `RUNNING` 且无完成事件的调用应进入恢复检查，而不是直接重试。
- Artifact 使用内容哈希命名，数据库保存元数据和引用。
- SQLite 启用 WAL、Foreign Keys 和 Busy Timeout。
- 数据库写操作通过 Repository 统一管理，避免业务模块各自创建写连接。
- 文件写入采用“临时文件 → `fsync` → 原子重命名”，数据库只在文件落盘成功后提交引用。
- Artifact 与记忆文件使用内容哈希或版本号，避免覆盖时产生半写状态。

### 15.2 云端部署边界

- 一个 SQLite 数据库只能由一个 wbot 服务实例负责写入。
- 可以在同一实例内并行执行任务节点，但 SQLite 同一时刻仍只有一个写事务。
- 不把同一个 SQLite 文件通过 NFS 或共享卷同时挂载给多个服务副本。
- 云端 V1 采用单实例或主备冷切换，不做共享数据库的水平扩容。
- 需要扩容时优先按用户或 Workspace 分片，每个实例使用独立的 `WBOT_DATA_ROOT` 和 SQLite 数据库。
- HTTP 层需要把同一 Workspace 的请求路由到拥有该分片的实例。
- 定期对 SQLite 和文件目录生成一致性备份，并实际验证恢复流程。

## 16. 异常处理

### 16.1 分类

| 类型 | 示例 | 默认处理 |
|---|---|---|
| 可重试基础设施错误 | 429、临时过载、网络超时 | 指数退避并限制次数 |
| 参数错误 | Schema 不合法 | 返回模型修正，不自动重复原调用 |
| 权限错误 | 越界路径、策略拒绝 | 返回结构化拒绝原因 |
| 业务拒绝 | 外部服务不接受操作 | 交给 Planner 重新规划 |
| 不确定结果 | 超时但外部请求可能成功 | 查询状态，禁止盲目重试 |
| 上下文超长 | 模型拒绝请求 | Reactive compaction 后重试一次 |

### 16.2 重试约束

- 重试策略由错误类型和工具配置共同决定。
- 非幂等工具没有幂等键时不得自动重试。
- 达到节点最大重试次数后交给 Planner 或 Advisor。
- 达到任务级失败预算后进入 `FAILED` 或请求人工干预。

## 17. 可观测性与审计

每个事件至少记录：

```json
{
  "event_id": "event_01",
  "trace_id": "trace_01",
  "task_id": "task_01",
  "node_id": "node_01",
  "type": "tool.completed",
  "actor": "agent",
  "timestamp": "...",
  "payload": {},
  "redaction_version": 1
}
```

关键指标：

- 任务成功率与平均完成时长。
- 节点重试率和重新规划率。
- Advisor 调用率、采纳率和费用增量。
- 不同模型的 Token、延迟和费用。
- Prompt Cache 命中率。
- 上下文压缩次数与压缩比例。
- 审批请求数、通过率和等待时长。
- 记忆检索命中率及错误记忆反馈。
- 工具错误率和不确定结果数量。

日志写入前必须脱敏，不得保存密钥、完整认证头或未遮蔽的敏感配置。

## 18. 安全要求

- 所有工具调用必须经过 Tool Registry 和 Permission Engine，禁止旁路执行。
- 文件路径在执行前解析为绝对路径并检查是否处于授权根目录。
- Shell 工具需要独立超时、输出限制和工作目录限制。
- 外部请求需要域名或服务作用域控制。
- Profile、记忆和历史中的文本均视为不可信输入，不能覆盖系统权限政策。
- 工具结果中的指令性文本不能自动提升为 System Prompt。
- 高成本 Advisor 获得的上下文遵循最小必要原则。
- Approval 和 Tool Call 使用不可预测 ID，并校验任务归属。
- 云端 API 必须认证，并在所有 Task、Session、Approval 和 Artifact 查询中执行租户隔离。
- 桌面客户端连接云端时使用 HTTPS，不允许通过 URL 参数传递长期 Token。
- Local Companion 采用设备注册、短期凭据和双向身份校验；本地 Permission Engine 的拒绝结果不能被云端覆盖。

## 19. 测试策略

### 19.1 单元测试

- Profile 合并顺序和 Schema 校验。
- Prompt 组装顺序与 Token 预算。
- 任务图状态转换和环检测。
- 权限矩阵与参数变更后的审批失效。
- 记忆去重、冲突和过期规则。
- 历史压缩不丢失任务约束。
- 重试、幂等和错误分类。

### 19.2 集成测试

- 低成本模型调用 Advisor 后继续完成任务。
- 审批模式下任务暂停、批准并恢复。
- 完全访问模式下跳过审批但保留审计事件。
- 进程中断后从 Checkpoint 恢复。
- 超长工具结果写入 Artifact 并生成快照。
- 节点失败后重新规划且不重复已完成副作用。

### 19.3 端到端场景

1. 读取项目文件，生成报告并保存。
2. 收集信息、生成草稿、等待发送审批。
3. 长任务运行中断后恢复。
4. 连续失败后咨询 Advisor 并修正方案。
5. 从历史项目决策中检索记忆完成新任务。

## 20. 分阶段实施计划

### Phase 1：可运行内核

- 配置加载和 Profile Schema。
- Go Agent Runtime 和 HTTP API。
- 通过 OpenAI 兼容协议接入 `deepseek-v4-flash`。
- Tool Registry 和基础文件工具。
- SQLite 事件日志。
- `approval` / `full_access` 权限模式。
- 最小可用 React Web UI 和 SSE 事件流。

### Phase 2：持久化工作状态

- Task、TaskNode 和 DAG 校验。
- Scheduler、Checkpoint 和任务恢复。
- 节点级验收和有限重试。
- Artifact Store。

### Phase 3：上下文与记忆

- Message Store 和 Token Budget。
- Tool Snapshot 与 History Summary。
- 记忆索引、分类文件和按需加载。
- 记忆提取、冲突处理和合并。

### Phase 4：混合模型

- Advisor Tool。
- 接入 `deepseek-v4-pro`。
- 调用预算和防递归限制。
- 失败触发策略。
- 模型成本与效果评测。

### Phase 5：桌面化与生产化

- 使用 Wails v2 封装同时支持 Local/Cloud 连接的桌面客户端。
- 消息渠道审批、定时调度和外部事件入口。
- 指标、报警和审计查询。
- SQLite 与本地文件系统的备份、恢复和 Workspace 分片。
- 单实例内的多 Worker 调度。
- 更细粒度的组织级权限策略。
- 后续实现云端调度 + Local Companion。

## 21. V1 验收标准

满足以下条件时，可以认为 V1 技术闭环完成：

- 修改 YAML Profile 后，下一次模型调用使用新配置并记录版本。
- 普通任务只调用低成本模型。
- 低成本模型能够主动调用 Advisor，且任务级调用上限生效。
- Agent 能保存、检索、修正和删除长期记忆。
- 超过上下文预算时能压缩历史，并可通过 Artifact 引用读取完整结果。
- Agent 能创建至少包含依赖关系的任务 DAG，并按照依赖顺序执行。
- 资源不冲突的就绪节点可以在配置的并行度内并发执行。
- Agent 进程重启后能够恢复未完成任务。
- HTTP API 可以创建 Workspace、Session、提交消息和查询任务状态。
- Web UI 可以接收流式事件并展示会话、任务图和文件变化。
- 同一套 UI 可以连接本地 Agent 或云端 Agent，业务组件不依赖具体传输方式。
- `approval` 模式下普通工作区写入自动允许，高风险工具暂停并通过 Web UI 或消息渠道等待审批。
- `full_access` 模式下无需审批，但仍产生完整审计日志。
- 非幂等工具不会因网络超时被盲目重复执行。
- 未通过顶层验收标准的任务不能被标记为完成。

## 22. 已确认决策

| 决策项 | V1 结论 |
|---|---|
| 编程语言与异步模型 | Go；Goroutine、Channel、`context.Context` 和有界 Worker Pool |
| 模型协议 | OpenAI 兼容协议 |
| 模型 | `deepseek-v4-flash` 默认执行，`deepseek-v4-pro` 作为 Advisor 工具 |
| 服务入口 | 提供 HTTP API |
| 审批入口 | Web UI 和消息渠道，共用 Approval Service |
| 记忆检索 | 索引文件 + 分类记忆文件，由低成本模型选择后按需加载；V1 不默认使用向量检索 |
| 普通工作区写入 | `approval` 模式下默认 `ALLOW` |
| 任务并行 | 无依赖且资源不冲突的节点允许并行执行 |
| UI | React + TypeScript；浏览器与 Wails v2 复用前端，桌面客户端支持 Local/Cloud 连接 |
| 部署 | 同时支持本地一体化和云端 Agent Service；后续扩展 Local Companion |
| 云端存储 | SQLite + 本地文件系统；单实例写入，使用持久卷和一致性备份 |

## 23. 相关官方资料

- DeepSeek API 使用 OpenAI 兼容格式：<https://api-docs.deepseek.com/>
- DeepSeek 模型列表接口：<https://api-docs.deepseek.com/api/list-models/>
- Wails v2 文档：<https://wails.io/docs/introduction/>
- Wails 前端与 Go Binding：<https://wails.io/docs/guides/frontend/>
- React 官方文档：<https://react.dev/>

## 24. V1 实现映射

| 设计能力 | 实现位置 |
|---|---|
| Agent 主循环、验收、上下文压缩与恢复 | `internal/agent/service.go`、`internal/agent/evaluator.go`、`internal/history/manager.go` |
| OpenAI 兼容模型、重试与 Advisor | `internal/model/openai.go` |
| 记忆索引、分类文件与 CRUD | `internal/memory/manager.go` |
| SQLite、Checkpoint、审批、Artifact、幂等记录 | `internal/storage/store.go` |
| DAG 校验、状态机、并发调度与资源锁 | `internal/task/graph.go` |
| 权限与 Workspace 路径防逃逸 | `internal/permission/permission.go` |
| HTTP、SSE、Cookie 认证 | `internal/httpapi/server.go` |
| React 会话、任务图与审批界面 | `web/src/main.tsx` |
| Wails 桌面入口与 Local/Cloud HTTP Adapter | `cmd/wbot-wails/main.go`、`web/src/client.ts` |

V1 已实现低成本模型主循环、高成本 Advisor 工具及调用预算、Profile 热加载和版本事件、分类记忆、上下文预算与两级压缩、超大工具结果 Artifact、任务节点依赖、有界并发和资源锁、审批暂停与恢复、Checkpoint、最终验收、工具幂等记录、本地/云端 UI、Wails 入口和 SQLite/文件系统持久化。自动化测试覆盖模型兼容与重试、DAG 环检测和并发、审批恢复、上下文压缩、记忆 CRUD/版本/敏感信息、幂等、权限和路径逃逸。
