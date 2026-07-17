# V1 验收记录

本记录按 `TECH.md` 第 21 节逐项映射实现和自动化证据。

| 验收项 | 实现证据 | 测试证据 |
|---|---|---|
| Profile 热更新与版本记录 | 每轮加载 Profile，写入 `prompt.built` 版本、hash 和 Token 事件 | `internal/config/settings_test.go` |
| 默认低成本模型 | Runtime 只持有默认 Generator；Advisor 仅作为工具 | Agent 集成测试 |
| Advisor 与调用上限 | `consult_advisor`、任务级预算和独立模型 | `internal/tool/registry_test.go` |
| 长期记忆 CRUD | YAML 索引、分类 Markdown、版本、软删除、敏感信息阻断 | `internal/memory/manager_test.go` |
| 上下文预算和 Artifact | History Summary、Reactive compaction、大文件 Artifact | `internal/history/manager_test.go` |
| 任务 DAG | 四节点 DAG、依赖校验、确定性状态转换 | `internal/task/graph_test.go` |
| 有界并发和资源锁 | Scheduler semaphore 与按资源互斥锁 | `internal/task/graph_test.go` |
| 重启恢复 | Checkpoint、运行任务恢复、工具不确定结果保护 | Agent、Storage、Tool 测试 |
| HTTP API | Workspace、Session、Message、Task、Approval、Artifact、Memory、Metrics | 运行时冒烟测试 |
| SSE 与任务图 UI | Last-Event-ID 续传，UI 查询并展示节点 | Web 生产构建 |
| Local/Cloud UI | `AgentClient` Adapter、Wails Handler、远程 URL 模式 | Wails 与 Web 构建 |
| approval 权限 | L1 写入允许，L2 暂停，参数摘要绑定审批 | Agent 审批恢复测试 |
| full_access 审计 | 跳过审批，保留模型、工具、状态事件及模型用量 | Permission、Storage 测试 |
| 非幂等保护 | `tool_calls` 状态；相同参数返回缓存或不确定结果 | `internal/tool/registry_test.go` |
| 顶层验收 | 持久化标准；Evaluator 通过后才完成 | `internal/agent/evaluator_test.go` |

统一验收命令：

```bash
go test ./...
go vet ./...
cd web && npm run build
go build -tags wails -o ../bin/wbot-wails ../cmd/wbot-wails
```
