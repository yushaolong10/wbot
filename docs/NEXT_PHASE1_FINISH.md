# wbot P0/P1/P2 基础设施实施文档

> 状态：P0/P1 核心路径已实施，P2 动态规划待继续
> 日期：2026-07-19  
> 基于：[NEXT.md](./NEXT.md) 第 2.2、2.3、5、6 章节  

## 概述

本轮完成了验收、强类型任务图和图修订的基础设施。阶段编号按本文件的任务图改造口径描述；`NEXT.md` 中单独定义的 Profile v2 / Prompt Compiler 尚未实施。

| 阶段 | 目标 | 状态 |
|------|------|------|
| **P0** | 基于证据的验收系统 + Completion Gate | ✅ 核心路径完成，领域标准生成待扩展 |
| **P1** | Task Graph 强类型化 + 去标题依赖 | ✅ 已完成 |
| **P2** | Planner + Graph Revision + 自动重规划 | ⚠️ Template Planner 已完成，目标驱动 Planner 待实施 |
| P3 | Episode 与 Durable Scheduler | 待实施 |
| P4 | 数字人持续关系能力 | 待实施 |

---

## 一、新增文件

```
internal/agent/
  verifier.go              — 确定性验证器注册表 + 证据收集器
  verifier_test.go         — 验证器单元测试
  completion_gate.go       — 唯一任务完成入口
  planner.go               — 模板任务图生成器与 Revision 管理

internal/task/
  graph_validator.go       — Graph Proposal 校验 + 自动重规划条件
  graph_validator_test.go  — 图校验器单元测试
```

## 二、修改文件

### 2.1 `internal/domain/types.go` — 类型体系扩展

新增类型：

```go
// P1: Node 类型标识
type NodeKind string
const (
    NodeResearch  NodeKind = "research"
    NodePlan      NodeKind = "plan"
    NodeExecute   NodeKind = "execute"
    NodeVerify    NodeKind = "verify"
    NodeApproval  NodeKind = "approval"
    NodeWait      NodeKind = "wait"
    NodeLoadHist  NodeKind = "load_history"
    NodeRetrieve  NodeKind = "retrieve_memory"
)

// P1: Node 结构体扩展
// 新增字段: Kind, Inputs, Outputs, CriteriaIDs, Timeout, AssignedRole

// P0: 结构化验收标准
type CriterionType string
const (
    CriterionFileExists   CriterionType = "file_exists"
    CriterionFileContains CriterionType = "file_contains"
    CriterionCommand      CriterionType = "command"
    CriterionJSONSchema   CriterionType = "json_schema"
    CriterionArtifact     CriterionType = "artifact"
    CriterionToolResult   CriterionType = "tool_result"
    CriterionHTTPResponse CriterionType = "http_response"
    CriterionModelRubric  CriterionType = "model_rubric"
    CriterionUserApproval CriterionType = "user_approval"
)

type AcceptanceCriterion struct {
    ID, TaskID, NodeID  string
    Type                CriterionType
    Description         string
    Required            bool
    Config              json.RawMessage
    Status              string   // pending | passed | failed
    EvidenceIDs         []string
    Reason              string
}

// P0: 证据数据
type Evidence struct {
    ID, TaskID, NodeID, CriterionID string
    Type, Source, Digest, Summary   string
    ArtifactID                      string
    Passed                          bool
    CollectedAt                     time.Time
}

// P2: 图提案与修订
type NodeProposal struct { ... }
type GraphProposal  struct { ... }
type GraphRevision  struct { ... }

// P0: Completion Gate 动作
type GateAction string
const (
    ActionComplete        GateAction = "complete"
    ActionRetry           GateAction = "retry"
    ActionReplan          GateAction = "replan"
    ActionWaitForUser     GateAction = "wait_for_user"
    ActionWaitForExternal GateAction = "wait_for_external"
    ActionFail            GateAction = "fail"
)
```

### 2.2 `internal/storage/store.go` — 存储层扩展

**新增数据库表：**

```sql
-- P0: 证据表（traceable evidence）
CREATE TABLE evidence (
    id TEXT PRIMARY KEY,
    task_id, node_id, criterion_id TEXT NOT NULL,
    type, source, digest, summary TEXT NOT NULL,
    artifact_id TEXT DEFAULT '',
    passed INTEGER DEFAULT 0,
    collected_at TEXT NOT NULL
);

-- P2: 图修订版本表
CREATE TABLE graph_revisions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    reason, planner_model, nodes_json TEXT NOT NULL,
    source_event TEXT DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE(task_id, version)
);

-- P0: 强类型验收标准表
CREATE TABLE acceptance_criteria (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    node_id TEXT DEFAULT '',
    type TEXT NOT NULL,
    description TEXT DEFAULT '',
    required INTEGER DEFAULT 1,
    config TEXT DEFAULT '{}',
    status TEXT DEFAULT 'pending',
    reason TEXT DEFAULT '',
    created_at TEXT NOT NULL
);
```

**task_nodes 表新增列：** `kind`, `inputs`, `outputs`, `criteria_ids`, `timeout_ms`, `assigned_role`

**新增方法（15 个）：**

| 方法 | 用途 |
|------|------|
| `SaveEvidence` / `EvidenceByTask` / `EvidenceByNode` | 证据 CRUD |
| `CreateAcceptanceCriterion` / `CreateAcceptanceCriteriaBatch` | 批量创建验收标准 |
| `AcceptanceCriteria` / `UpdateAcceptanceCriterion` | 验收标准读写 |
| `NextGraphRevision` / `SaveGraphRevision` | 图版本管理 |
| `LatestGraphRevision` / `GraphRevisions` | 版本查询 |
| `TaskArtifacts` | 按任务查询 artifact |

**向后兼容：** `Criteria()` / `SetCriterion()` 保留，支持从旧 `task_criteria` 表回退读取。

### 2.3 `internal/agent/verifier.go` — 确定性验证器系统

**架构：**

```
EvidenceCollector
  └─ VerifierRegistry
       ├─ FileExistsVerifier      (file_exists)
       ├─ FileContainsVerifier    (file_contains)
       ├─ CommandVerifier         (command)
       └─ ArtifactVerifier        (artifact)
```

**设计原则：**
- 确定性验证器先执行，结果写入 Evidence 表
- 模型语义评估（model_rubric）在确定性验证之后
- 确定性失败不可被模型推翻

**验证器配置示例：**

```yaml
# file_exists 验证器
type: file_exists
config:
  path: reports/final.md

# command 验证器
type: command
config:
  command: make test
  expected_exit_code: 0

# file_contains 验证器
type: file_contains
config:
  path: README.md
  content: "## Installation"
```

### 2.4 `internal/agent/completion_gate.go` — 完成门控

**唯一可写入 `completed` 状态的入口。** Executor 和模型不能直接完成任务。

**判定流程：**

```text
AcceptanceCriteria
  → EvidenceCollector.Collect()    // 收集确定性证据
  → Gate.Evaluate()
       ├─ 所有 Required 标准 passed → ActionComplete
       ├─ 任一 Required 标准 failed → ActionFail
       ├─ 有 pending 的 Required   → ActionRetry
       ├─ 确定性证据失败           → ActionRetry（不可被模型推翻）
       └─ 全部通过               → ActionComplete
```

### 2.5 `internal/agent/planner.go` — 模板图生成器

当前 Planner 根据关键词复杂度选择 simple/complex 模板，并不是由模型根据目标生成任意 GraphProposal。GraphProposal 校验、持久化和 Revision 机制已经就绪，可作为后续目标驱动 Planner 的安全边界。

**支持两种复杂度：**

| 复杂度 | 节点图 |
|--------|--------|
| `simple` | load_history → retrieve_memory → execute → verify |
| `complex` | load_history → retrieve_memory → research → plan → execute → verify |

**Replan 机制：**
- 复用已完成的历史加载、记忆检索和调研节点 ID
- 重新生成规划、执行和验收节点
- 新旧节点合并后写入
- GraphRevision 版本号递增
- 最多允许三次 replan，避免固定路线无限循环

**GraphRevision 记录：**
```json
{
  "version": 2,
  "reason": "node_failed: 执行变更连续失败3次",
  "planner_model": "planner-v1",
  "nodes_json": "{...}"
}
```

### 2.6 `internal/task/graph_validator.go` — 图提案校验器

**`ValidateGraphProposal` 检查项：**

1. 节点数不为空，不超过上限（默认 12）
2. 每个节点有唯一 `temp_id`
3. `kind` 为合法枚举值
4. `risk_level` 合法（low/medium/high/critical）
5. 所有依赖的 `temp_id` 存在
6. 无循环依赖
7. 每个 execute 节点至少声明一个 output 或 criterion
8. verify 节点不能依赖 verify 节点（逻辑约束）

**`ShouldReplan` 触发条件：**

| 条件 | 触发 |
|------|------|
| `ReplanNodeFailed` | 同一节点尝试次数 ≥ `replanAfterFailures` |
| `ReplanAssumptionWrong` | 新证据推翻关键假设 |
| `ReplanUserChanged` | 用户修改目标 |
| `ReplanEvaluatorReplan` | Evaluator 返回 replan |

### 2.7 `internal/agent/service.go` — Run 循环重构

**Start() 变更：**

```go
// 旧代码：硬编码 4 节点
h := storage.NewID("node")
m := storage.NewID("node")
x := storage.NewID("node")
v := storage.NewID("node")
nodes := []domain.Node{
    {Title: "加载会话历史", ...},
    {Title: "检索长期记忆", ...},
    {Title: "执行目标", ...},
    {Title: "验收结果", ...},
}

// 新代码：Planner 动态生成
nodes, revision, err := s.planner.GenerateGraph(ctx, t, "simple")
```

**Run() 关键变更：**

| 旧行为 | 新行为 |
|--------|--------|
| `if n.Title == "执行目标"` | `if n.Kind == domain.NodeExecute`（保留 Title fallback） |
| `if n.Title == "验收结果"` | `if n.Kind == domain.NodeVerify` |
| 直接 `UpdateTask(completed)` | `gate.Evaluate()` → 按 GateResult.Action 执行 |
| 验收失败 → 注入提示继续循环 | 验收失败 → Gate 判定 retry/replan/fail |
| 无重规划 | `ActionReplan` → `planner.Replan()` 生成新图 |

### 2.8 `internal/httpapi/server.go` — API 扩展

`GET /api/v1/tasks/:id` 响应新增字段：

```diff
  {
    "task": {...},
    "nodes": [...],
    "timing": {...},
    "acceptance_criteria": [...],
+   "evidence": [...],
+   "graph_revisions": [...]
  }
```

### 2.9 前端 `web/src/main.tsx` — UI 任务图适配

**类型扩展：**
- `Node` 类型新增 `Kind`, `Description`, `Outputs`, `CriteriaIDs`, `RiskLevel`, `Attempt`, `MaxAttempts`, `AssignedRole`
- 新增 `AcceptanceCriterion`, `Evidence`, `GateResult` 类型

**状态管理：**
- 新增 `criteria` / `evidence` 状态变量
- 任务详情请求后同步更新
- 切换会话时清除

**RightSidebar 节点渲染：**

```
┌─────────────────────────────────────────────┐
│ [执行] npm install         running          │
│        依赖 1 项 · 排队 2s · 执行 15s       │
│                                             │
│ [验收] 验证安装结果         pending         │
│        依赖 1 项 · 排队 0s                  │
└─────────────────────────────────────────────┘
```

每个节点左侧显示 Kind 彩色徽章：

| Kind | 中文 | 背景色 |
|------|------|--------|
| `research` | 调研 | 蓝 `#e8f0fe` |
| `plan` | 规划 | 黄 `#fef7e0` |
| `execute` | 执行 | 绿 `#e6f4ea` |
| `verify` | 验收 | 紫 `#f3e8fd` |
| `approval` | 审批 | 红 `#fce8e6` |
| `wait` / `load_history` / `retrieve_memory` | 等待/历史/记忆 | 灰 |

**验收标准面板（可折叠）：**

```
▼ 验收标准 (2/3 通过)
  ✓ 模型返回非空交付结果       passed
  ✗ 不存在未处理的工具错误     failed  原因：最后一次工具调用返回error
  ○ 测试通过                   pending
```

**证据面板（可折叠）：**

```
▼ 证据 (2 条)
  ✓ command       命令 "make test" 退出码 0 (期望 0)
  ✗ file_exists   文件 reports/final.md 不存在
```

**风险等级：** 执行节点右侧显示红色风险标签（`low`/`medium`/`high`/`critical`）。

---

## 三、关键设计原则

1. **模型不能推翻确定性失败。** 命令退出码为 1 时，语义 Evaluator 不能判定通过。

2. **只有 Completion Gate 可以写入 `completed`。** `service.go` 中不再存在直接调用 `store.UpdateTask(completed)` 的路径。

3. **Kind 优先，Title 降级。** 运行时优先使用 `Node.Kind` 判断节点用途，仅当 Kind 为空时 fallback 到 Title 字符串匹配。

4. **Graph Revision 不可变。** 重规划时旧版本保留，已完成节点及证据不被覆盖。

5. **Planner / Executor / Evaluator 各司其职。** 当前 Template Planner 生成受校验的模板图，Executor 执行节点，Completion Gate 判定完成。

6. **向后兼容。** 旧 `task_criteria` 表仍可读取；旧节点（无 Kind 字段）通过 Title fallback 正常工作。

---

## 四、测试覆盖

测试覆盖包括文件/命令/工具结果验证、Workspace 越界拒绝、未审批命令拒绝、Gate fail/retry/replan 分流、Revision 节点复用、必需验收契约保留、历史摘要字段保真，以及原有服务集成测试。

---

## 五、下一步（P3 / P4）

按 NEXT.md 实施顺序，待完成：

- **P3：Episode 与 Durable Scheduler** — Wakeup 表、Episode 表、SQLite Lease、多 Worker、崩溃恢复
- **P4：数字人持续关系能力** — Mission、Commitment、用户关系模型、多角色、主动汇报
