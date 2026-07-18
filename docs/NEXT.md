# wbot 下一阶段：复杂长时任务与数字人配置体系

> 状态：头脑风暴与方案草案  
> 日期：2026-07-18  
> 目标：将 wbot 从“带持久化与记忆的单次 ReAct Agent”演进为可配置、可恢复、可验证、可长期运行的数字工作者 Runtime。

## 1. 背景与结论

wbot 当前已经具备长时 Agent 的重要基础设施：Profile、Task Graph、Checkpoint、Approval、Event、分层历史压缩、长期记忆、Artifact、工具幂等和中断恢复。但目前的运行方式仍以一次任务内的连续模型循环为中心，复杂任务被压缩进单个执行节点，Profile 中的许多字段没有转化为真实行为，任务完成也缺少领域证据。

下一阶段不应继续扩大单一 System Prompt，而应把 Prompt 当作运行时编译产物，将人格、制度、角色、技能、任务契约、运行状态、记忆和本次事件分别管理。

建议采用统一的新运行链路：

```text
Profile Compiler
  → Mission Contract
  → Planner 生成动态 Graph
  → Durable Scheduler 分 Episode 唤醒
  → Executor 执行单个 Node
  → Evidence Collector 收集证据
  → Evaluator 验收
  → 完成 / 重试 / 重规划 / 等待
```

## 2. 当前实现的主要问题

### 2.1 Profile 大部分配置没有形成真实行为

当前 Profile 声明了 `working_style`、`memory`、`constraints` 等字段，但 Context Builder 真正注入模型的主要是：

- `identity.name`
- `identity.role`
- `identity.language`
- `personality.tone`
- `custom_instructions`

`initiative`、`plan_before_execution`、`ask_when_ambiguous` 等字段目前更多是配置描述，没有编译成明确的 Prompt 协议或 Runtime 策略。

### 2.2 Task Graph 仍是固定流程壳

每个任务固定创建以下节点：

```text
加载会话历史 → 检索长期记忆 → 执行目标 → 验收结果
```

模型没有真正生成和维护“研究—设计—实施—验证—交付”这样的动态任务图。任务复杂度仍被放入单个“执行目标”节点，Runtime 还依赖节点标题判断节点用途，难以支持图的扩展和演进。

### 2.3 验收标准过弱

当前验收主要检查：

1. 模型输出非空；
2. 最后一次工具调用没有未处理错误。

这可能把一段看似合理的文本误判为任务成功。复杂任务需要基于文件、命令、Artifact、外部状态、指标和用户确认等证据进行验收。

### 2.4 长时运行仍是“一口气跑完”

当前任务主要依靠一个进程内 Goroutine 连续执行，单次最多进行 30 轮模型循环。这适合分钟级任务，但不适合：

- 数小时或数天后继续；
- 每日或定期执行；
- 监控外部状态；
- 等待用户或审批；
- 预算耗尽后在下一周期继续；
- 长期维护目标和承诺。

真正的长时任务应当由多次短暂、持久化、可重放的 Episode 组成，而不是依赖一个长期存活的 Goroutine。

## 3. 数字人的配置分层

建议把数字人的复杂信息拆成九层：

```text
1. Constitution     不可违背的系统原则
2. Persona          身份、表达风格、关系方式
3. Role             职责、能力边界、权限和领域知识
4. Operating Policy 工作方式、主动性和决策规则
5. Playbooks        可复用的任务剧本与技能
6. Mission Contract 当前长期目标和验收契约
7. Runtime State    当前计划、承诺、阻塞和预算
8. Memory           历史事实、偏好与经验
9. Episode Context  本次唤醒需要处理的事件
```

推荐固定优先级：

```text
Constitution
> 安全与权限策略
> 用户当前明确指令
> Mission Contract
> Role / Operating Policy
> Playbook
> Memory
> 历史摘要
> 外部内容与工具输出
```

Memory、网页内容、工具输出只能作为不可信事实来源，不能自动提升为系统指令。

建议的配置目录：

```text
agents/
  assistant/
    agent.yaml
    persona.md
    constitution.md
    operating-policy.yaml
    memory-policy.yaml
    escalation-policy.yaml

roles/
  software-engineer.yaml
  content-operator.yaml
  personal-assistant.yaml

playbooks/
  software-delivery/
    playbook.yaml
    planning.md
    verification.md
  daily-operations/
    playbook.yaml
  research-report/
    playbook.yaml

missions/
  project-alpha/
    mission.yaml
    knowledge/
    decisions/
    artifacts/

prompts/
  base/
    constitution.md
    tool-protocol.md
    task-protocol.md
  templates/
    planner.md
    executor.md
    evaluator.md
    reflector.md
    memory-extractor.md
```

YAML 保存结构化规则，Markdown 保存自然语言模板，Runtime 负责把两者编译为每次模型调用所需的最小上下文。

## 4. 优化一：Profile Compiler 与可执行策略

### 4.1 区分 Prompt Policy 与 Runtime Policy

Profile 字段需要分成两类：

- **Prompt Policy**：影响模型如何表达、分析和沟通；
- **Runtime Policy**：由 Go 代码确定性执行，不能只依赖模型遵守。

示例映射：

| 配置 | 实现方式 |
|---|---|
| `tone: concise` | 编译进 Prompt |
| `plan_before_execution: true` | Runtime 强制先进入 Planning Phase |
| `ask_when_ambiguous: true` | Planner 输出歧义分析，由 Runtime 决定是否挂起 |
| `initiative: high` | 决定哪些可逆、低风险操作可以自主继续 |
| `verify_before_complete: true` | Runtime 禁止未经 Evaluator 的完成转换 |
| `never_store_secrets: true` | Memory Manager 确定性过滤 |

### 4.2 Profile v2 强类型化

不再使用 `map[string]any` 表示重要策略：

```go
type ProfileV2 struct {
    Version       int
    Identity      IdentityConfig
    Communication CommunicationPolicy
    Autonomy      AutonomyPolicy
    Planning      PlanningPolicy
    Verification  VerificationPolicy
    Memory        MemoryPolicy
    Escalation    EscalationPolicy
    PromptModules []PromptModuleRef
}

type AutonomyPolicy struct {
    Level                  string
    AllowReversibleActions bool
    AllowScopeExpansion    bool
    MaxRiskWithoutApproval string
}

type PlanningPolicy struct {
    RequiredForComplexTasks bool
    ComplexityThreshold     int
    ReplanAfterFailures     int
    MaxActiveNodes          int
}

type VerificationPolicy struct {
    Required              bool
    AllowModelOnly        bool
    RequireEvidence       bool
    RequireUserAcceptance bool
}
```

继续使用 YAML `KnownFields(true)`，让拼错或未支持的字段直接报错。

### 4.3 Prompt Compiler

新增：

```text
internal/prompt/
  compiler.go
  bundle.go
  renderer.go
  policy.go
```

Compiler 输出：

```go
type PromptBundle struct {
    Version       string
    Blocks        []PromptBlock
    RuntimePolicy RuntimePolicy
    Hash          string
}

type PromptBlock struct {
    ID       string
    Priority int
    Stable   bool
    Content  string
    Tokens   int
}
```

`RuntimePolicy` 交给 Service、Scheduler、Memory Manager 和 Permission Engine 使用，`Blocks` 才发送给模型。

每次编译应记录：

```json
{
  "prompt_bundle_version": "2.3.1",
  "template_versions": {
    "constitution": "1.2",
    "executor": "2.1",
    "software_delivery_playbook": "3.0"
  },
  "profile_hash": "...",
  "mission_revision": 7,
  "included_memory_ids": [],
  "token_breakdown": {}
}
```

### 4.4 Profile 示例

```yaml
version: 2

agent:
  id: alex
  display_name: Alex
  persona_ref: personas/alex.md
  constitution_ref: prompts/base/constitution.md

roles:
  - software_engineer
  - project_operator

operating_policy:
  initiative:
    level: high
    act_without_confirmation_when:
      - reversible
      - within_existing_scope
      - no_external_communication
    must_escalate_when:
      - scope_expansion
      - destructive_action
      - external_commitment
      - ambiguous_business_decision

  planning:
    required_for_complex_tasks: true
    replan_after_failures: 2
    max_active_goals: 3
    max_step_duration: 30m

  communication:
    progress_interval: 15m
    report_on:
      - milestone_completed
      - blocked
      - approval_required
      - material_plan_change

runtime:
  max_model_rounds_per_episode: 8
  max_episode_duration: 10m
  daily_token_budget: 500000
  idle_behavior: suspend
  heartbeat: 30m

memory_policy:
  write:
    user_preferences: explicit_or_repeated
    project_decisions: always
    speculative_facts: never
    secrets: never
  retrieve:
    scopes: [mission, workspace, user]
    max_tokens: 6000
```

关键要求是每个配置项必须对应可测试的 Runtime 行为，不能只是告诉模型“应该这样做”。

## 5. 优化二：动态 Task Graph 与重规划

### 5.1 Node 使用明确类型

Runtime 不应通过标题字符串判断节点用途：

```go
type Node struct {
    ID           string
    TaskID       string
    Kind         NodeKind
    Title        string
    Description  string
    Status       NodeStatus
    DependsOn    []string
    Inputs       []ArtifactRef
    Outputs      []OutputContract
    Criteria     []AcceptanceCriterion
    RiskLevel    string
    Attempt      int
    MaxAttempts  int
    Timeout      time.Duration
    AssignedRole string
    Result       string
}

const (
    NodeResearch NodeKind = "research"
    NodePlan     NodeKind = "plan"
    NodeExecute  NodeKind = "execute"
    NodeVerify   NodeKind = "verify"
    NodeApproval NodeKind = "approval"
    NodeWait     NodeKind = "wait"
)
```

### 5.2 引入 Planner

任务创建后的流程改为：

```text
接收 Objective
  → 生成 Mission Contract 草案
  → 判断任务复杂度
  → 简单任务：创建单个 Execute Node
  → 复杂任务：Planner 生成 GraphProposal
  → Runtime 校验 GraphProposal
  → 保存 Graph Revision
  → Scheduler 执行 Ready Nodes
```

Planner 必须输出结构化结果，例如：

```json
{
  "goal_summary": "完成项目版本发布",
  "assumptions": [],
  "open_questions": [],
  "nodes": [
    {
      "temp_id": "inspect",
      "kind": "research",
      "title": "检查当前状态",
      "depends_on": [],
      "outputs": ["repository_state"]
    },
    {
      "temp_id": "implement",
      "kind": "execute",
      "title": "实现所需变更",
      "depends_on": ["inspect"],
      "criteria": [
        {
          "type": "command",
          "command": "make test",
          "expected_exit_code": 0
        }
      ]
    }
  ]
}
```

### 5.3 Runtime 校验 Planner 输出

模型提出计划，但 Runtime 保存前必须检查：

- 图无环且所有依赖存在；
- 节点数量不超过策略上限；
- 风险等级合法；
- 路径和资源没有越界；
- 每个执行节点至少声明一个输出或验收标准；
- 计划不能自行扩大权限、预算、deadline 或目标范围。

### 5.4 Graph Revision

长任务计划会随着证据变化，不应直接覆盖旧图：

```go
type GraphRevision struct {
    ID           string
    TaskID       string
    Version      int
    Reason       string
    PlannerModel string
    SourceEvent  string
    CreatedAt    time.Time
}
```

旧 revision 保留用于审计，已完成节点及已有证据不能在重规划时被悄悄改写。

### 5.5 自动重规划条件

以下情况可以触发重规划：

- 同一节点连续失败达到配置阈值；
- 新证据推翻关键假设；
- 用户修改目标；
- 外部等待超时；
- 依赖结果与预期输出不匹配；
- 剩余预算不足；
- Evaluator 返回 `replan`。

## 6. 优化三：基于证据的验收系统

推荐的新验收链路：

```text
Acceptance Contract
  → Evidence Collector
  → Deterministic Verifiers
  → Semantic Evaluator
  → Completion Gate
```

### 6.1 结构化验收标准

```go
type AcceptanceCriterion struct {
    ID          string
    TaskID      string
    NodeID      string
    Type        CriterionType
    Description string
    Required    bool
    Config      json.RawMessage
    Status      string
    EvidenceIDs []string
    Reason      string
}
```

第一批建议支持：

| 类型 | 含义 |
|---|---|
| `file_exists` | 文件存在 |
| `file_contains` | 文件包含指定内容 |
| `command` | 命令退出码和输出符合预期 |
| `json_schema` | JSON 满足 Schema |
| `artifact` | 产生指定类型 Artifact |
| `tool_result` | 指定工具成功 |
| `http_response` | HTTP 状态码或响应满足条件 |
| `model_rubric` | 语义质量评估 |
| `user_approval` | 用户明确确认 |

示例：

```yaml
acceptance_criteria:
  - id: tests-pass
    type: command
    required: true
    config:
      command: make test
      expected_exit_code: 0

  - id: report-created
    type: file_exists
    required: true
    config:
      path: reports/final.md

  - id: report-quality
    type: model_rubric
    required: true
    config:
      rubric:
        - 包含执行摘要
        - 包含风险
        - 所有结论带证据

  - id: user-acceptance
    type: user_approval
    required: true
```

### 6.2 Evidence 成为一等数据

```go
type Evidence struct {
    ID          string
    TaskID      string
    NodeID      string
    CriterionID string
    Type        string
    Source      string
    ArtifactID  string
    Digest      string
    Summary     string
    CollectedAt time.Time
}
```

命令验证证据示例：

```json
{
  "type": "command_result",
  "exit_code": 0,
  "stdout_artifact_id": "artifact_123",
  "command_digest": "...",
  "workspace_revision": "git-sha",
  "collected_at": "..."
}
```

验收顺序优先使用确定性验证器：

```text
文件 / 命令 / Schema / HTTP 验证
  → 收集证据
  → 模型进行语义验收
  → 用户验收（若要求）
```

模型不能推翻确定性失败。例如命令退出码为 1 时，语义 Evaluator 不能判定通过。

### 6.3 Completion Gate

Evaluator 返回动作应限制为：

```text
complete
retry
replan
wait_for_user
wait_for_external
fail
```

最终 `completed` 状态只能由 Completion Gate 写入。Executor 和模型不能直接完成任务。

## 7. 优化四：Episode 与 Durable Scheduler

### 7.1 长任务拆成短 Episode

```text
事件到达
  → Scheduler 领取任务租约
  → 执行一个 Episode
  → 持久化全部状态
  → 设置 next_wakeup_at
  → 释放租约并退出
```

```go
type Episode struct {
    ID             string
    MissionID      string
    TaskID         string
    NodeID         string
    TriggerType    string
    TriggerPayload json.RawMessage
    Status         string
    StartedAt      time.Time
    FinishedAt     time.Time
    NextWakeupAt   *time.Time
    ModelRounds    int
    InputTokens    int
    OutputTokens   int
    CheckpointID   string
}
```

每个 Episode 独立限制：

```yaml
max_duration: 10m
max_model_rounds: 8
max_tool_calls: 20
max_tokens: 100000
```

达到 Episode 限制时，任务不应直接失败，而应保存 Checkpoint、总结进展、安排下一次唤醒并退出当前 Episode。

### 7.2 持久化 Wakeup 队列

SQLite 初期即可满足：

```sql
CREATE TABLE wakeups (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    node_id TEXT,
    trigger_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    scheduled_at TEXT NOT NULL,
    status TEXT NOT NULL,
    lease_owner TEXT,
    lease_expires_at TEXT,
    attempts INTEGER NOT NULL DEFAULT 0
);
```

支持的触发来源：

```text
immediate       立即继续
timer           指定时间唤醒
user_message    用户回复
approval        审批完成
webhook         外部事件
poll            定期检查外部状态
budget_reset    新预算周期开始
dependency      依赖节点完成
```

### 7.3 数据库 Lease

当前进程内 `running map` 只能防止单进程重复执行，应改为数据库租约：

```text
Worker 领取 Wakeup
  → 设置 lease_owner 和 lease_expires_at
  → 执行期间定期续租
  → 完成后 ACK
  → 进程崩溃后租约过期，由其他 Worker 重新领取
```

工具调用继续使用幂等键。对于崩溃时结果未知的副作用操作，保持 `RESULT_UNKNOWN`，先检查外部状态再决定是否重试。

### 7.4 等待状态携带恢复条件

不能只保存 `waiting_external`，还要保存为什么等待、何时重试和超时后的动作：

```json
{
  "status": "waiting_external",
  "waiting_for": "deployment_completed",
  "resume_when": {
    "type": "poll",
    "interval": "10m",
    "timeout": "6h"
  },
  "next_wakeup_at": "...",
  "timeout_action": "escalate"
}
```

Memory maintenance、History compact、Commitment check、Mission review 和 Progress report 也可以统一建模为系统 Wakeup，从而获得审计、重试和限流能力。

## 8. Mission 与 Commitment

### 8.1 Mission 成为 Task 的上层实体

建议形成以下层级：

```text
Mission：持续数天或数月的长期目标
  └─ Task：一个阶段性成果
      └─ Node：可调度、可重试的工作单元
          └─ Episode：一次短暂的模型唤醒
              └─ Tool Call
```

Mission 示例：

```yaml
mission:
  id: maintain-project-alpha
  objective: 持续维护并交付 Project Alpha

success_criteria:
  - id: tests
    type: command
    command: make test
    expected_exit_code: 0
  - id: build
    type: command
    command: make build
    expected_exit_code: 0
  - id: user_acceptance
    type: approval
    required: true

constraints:
  deadline: 2026-08-30T18:00:00+08:00
  allowed_workspaces:
    - /workspace/project-alpha
  forbidden_actions:
    - publish_without_approval

budget:
  daily_tokens: 300000
  max_external_spend_cny: 0

cadence:
  heartbeat: 1h
  progress_report: daily
  reevaluate_plan: 6h

stop_conditions:
  - success_criteria_passed
  - user_cancelled
  - budget_exhausted
```

### 8.2 Commitment 独立于 Memory

数字人不仅需要知道历史，还要持续兑现承诺：

```yaml
commitment:
  id: report-friday
  description: 周五前提交项目报告
  owner: agent
  beneficiary: user
  due_at: 2026-07-24T18:00:00+08:00
  status: active
  source_message_id: msg_xxx
  next_check_at: 2026-07-23T10:00:00+08:00
```

三者语义不同：

- Memory：Agent 知道什么；
- Task：Agent 当前正在做什么；
- Commitment：Agent 承诺未来必须处理什么。

## 9. Prompt 的角色分离

不要让一个 Prompt 同时承担规划、执行、验收和记忆整理。即使复用同一个基础模型，也应构建不同视图：

```text
Planner Prompt
  = Constitution
  + Planning Policy
  + Mission Contract
  + Current Task Graph
  + Relevant Memory
  + Trigger Event

Executor Prompt
  = Constitution
  + Tool Protocol
  + Current Node Contract
  + Required Evidence
  + Minimal Relevant Context

Evaluator Prompt
  = Acceptance Criteria
  + Evidence Manifest
  + Artifact References
  + Failure Policy

Reflector Prompt
  = Episode Trace
  + Changed Facts
  + Open Commitments
  + Memory Write Policy
```

## 10. 推荐实施顺序

### P0：建立正确的完成门槛

- 结构化 Acceptance Criterion；
- Evidence 表与 Evidence Collector；
- Verifier Registry；
- Completion Gate；
- 禁止 Executor 或模型直接完成 Task。

P0 优先级最高，因为错误地宣称完成比暂时无法长期运行风险更大。

### P1：Profile v2 与 Prompt Compiler

- 强类型 Profile；
- Runtime Policy；
- Prompt 文件化和版本化；
- 配置字段到行为的映射测试；
- Prompt Bundle 审计记录。

### P2：动态 Planner 与 Graph Revision

- Node Kind；
- GraphProposal Schema；
- Planner；
- Graph Validator；
- 自动重规划；
- 去除基于节点标题的控制逻辑。

### P3：Episode 与 Durable Scheduler

- Wakeup 表；
- Episode 表；
- SQLite Lease；
- Timer、User、Approval、External 等触发器；
- 单 Episode 预算；
- 崩溃恢复和重复投递测试。

### P4：数字人的持续关系能力

- Mission；
- Commitment；
- 用户关系模型；
- 多角色切换；
- 主动汇报与提醒；
- 定期复盘与任务健康检查。

情绪或 Persona 状态只应影响表达方式，不应改变权限、安全边界和事实判断。

## 11. 目标运行形态

```text
用户提出目标
  ↓
Profile Compiler 生成运行策略
  ↓
创建 Mission Contract 和验收标准
  ↓
Planner 生成 Graph Revision 1
  ↓
Scheduler 唤醒 Research Node
  ↓
Episode 执行、保存证据、退出
  ↓
依赖满足后唤醒 Execute Node
  ↓
节点失败达到阈值，生成 Graph Revision 2
  ↓
等待外部事件，持久化休眠
  ↓
Webhook 或 Timer 再次唤醒
  ↓
确定性 Verifier 检查
  ↓
语义 Evaluator 复核
  ↓
Completion Gate 完成任务
```

## 12. 完成标准

下一阶段改造完成后，系统至少应满足：

1. Profile 中每个重要字段都能追踪到 Prompt 行为或 Runtime 行为；
2. 复杂任务可以生成、校验、修订和恢复动态任务图；
3. 任务完成必须附带与验收标准关联的持久化证据；
4. 模型、Executor 不能绕过 Completion Gate；
5. 长任务可以在进程退出后从 Episode 和 Wakeup 恢复；
6. 等待用户、审批、定时器和外部事件不占用长期 Goroutine；
7. 每次规划、执行、验收和重规划均记录 Prompt 版本、策略快照和输入证据；
8. 重复投递、进程崩溃和工具结果未知时不会盲目重复副作用；
9. Context 大小继续保持有界，不随 Mission 运行时间线性增长；
10. 系统能够解释某次决定来自哪一版 Profile、Mission、Plan、Memory 和 Evidence。
