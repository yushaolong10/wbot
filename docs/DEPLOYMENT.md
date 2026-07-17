# wbot 部署、配置与使用指南

本文对应 TECH.md 的 V1 实现，说明本地 Web、桌面启动器、Wails 原生窗口、Linux 服务和 Docker Compose 五种运行方式。
覆盖本地运行、容器部署、模型与权限配置、Web UI、HTTP API、数据备份和常见故障。

## 1. 先选择部署方式

| 场景 | 推荐方式 | 启动入口 | 数据位置 | 适用说明 |
|---|---|---|---|---|
| 本机开发、调试 | 本地 Web 服务 | `bin/wbot-server` | 本机目录 | 最容易排错，推荐首次运行使用 |
| 本机日常使用 | 浏览器桌面启动器 | `bin/wbot-desktop` | 本机目录 | 自动启动 Runtime 并打开系统浏览器 |
| 本机独立窗口 | Wails 原生窗口 | `bin/wbot-wails` | 本机目录 | 使用系统 WebView，构建环境要求更高 |
| 单机服务器 | systemd + Nginx/Caddy | `bin/wbot-server` | 服务器本地磁盘 | 适合长期运行和 HTTPS 访问 |
| 容器化单实例 | Docker Compose | 容器内 `wbot-server` | Docker Volume | 部署和迁移方便 |

当前实现使用 SQLite，一个数据库只能由一个 wbot 实例写入。以上方案都是单实例方案，不要让多个进程或容器共享同一个 `wbot.db`。

## 2. 运行要求与实现约束

源码构建需要：

- Go 1.21 或更高版本。
- Node.js 18 或更高版本，推荐 Node.js 20。
- DeepSeek API Key，或兼容 OpenAI Chat Completions 协议的模型服务。
- 构建原生 Wails 窗口时，还需要 Wails v2 对应操作系统的 WebView/GUI 构建依赖。

SQLite 使用 `modernc.org/sqlite` 纯 Go 驱动，不需要 CGO 或额外安装 SQLite 开发库。

当前二进制不会嵌入以下运行资源：

```text
web/dist/                 # Web UI
profiles/default.yaml     # 默认 Profile
```

因此，本地和裸机服务器部署必须从包含这些目录的应用目录启动，或者使用绝对路径设置 `WBOT_PROFILE`。只复制一个 `wbot-server` 二进制会导致 Profile 加载失败或只能看到 API 提示页。Docker 镜像已经复制了这些资源。

## 3. 统一构建流程

在仓库根目录执行：

```bash
cd /path/to/wbot
make build
```

`make build` 会依次执行：

1. `npm ci` 安装锁定版本的前端依赖；
2. TypeScript 与 Vite 构建，输出到 `web/dist/`；
3. 构建 `bin/wbot-server`；
4. 构建 `bin/wbot-desktop`。

完整验证：

```bash
make test
make build
```

不建议把 `npm install` 和 `npm run build` 拼成一行传给 `make`。直接执行 `make build` 即可。

## 4. 方案 A：本地 Web 服务

这是首次运行和开发调试的推荐方式。

### 4.1 最小配置

```bash
cd /path/to/wbot
make build

export WBOT_MODEL_API_KEY='your-api-key'
export WBOT_MODEL_BASE_URL='https://api.deepseek.com'
export WBOT_ADVISOR_MODEL='deepseek-v4-pro'
export WBOT_WORKSPACE_ROOT='/absolute/path/to/allowed/workspace'
export WBOT_DATA_ROOT="$HOME/.local/share/wbot"

./bin/wbot-server
```

默认情况下 Advisor 使用 `WBOT_MODEL_BASE_URL`。只有 Advisor 位于另一个兼容服务时才需要额外设置：

```bash
export WBOT_ADVISOR_BASE_URL='https://advisor-api.example.com'
```

启动成功后显示：

```text
wbot listening on http://127.0.0.1:8080
```

浏览器访问 <http://127.0.0.1:8080>，健康检查：

```bash
curl http://127.0.0.1:8080/healthz
```

### 4.2 权限模式选择

默认 `approval` 模式会对 Shell 等 L2 操作请求人工审批：

```bash
export WBOT_PERMISSION_MODE=approval
```

如果是完全可信的个人本机环境，希望跳过人工审批：

```bash
export WBOT_PERMISSION_MODE=full_access
```

`full_access` 会允许模型直接执行 Shell 命令，应只用于可信工作区。

### 4.3 前后端开发模式

终端 1：

```bash
cd /path/to/wbot
export WBOT_MODEL_API_KEY='your-api-key'
export WBOT_WORKSPACE_ROOT='/absolute/path/to/allowed/workspace'
go run ./cmd/wbot-server
```

终端 2：

```bash
cd /path/to/wbot/web
npm ci
npm run dev
```

Vite 开发服务器主要用于前端开发。正常使用请访问 Go 服务的 `8080` 端口；如从 Vite 端口访问，需要额外配置 API 地址或开发代理。

## 5. 方案 B：浏览器桌面启动器

`wbot-desktop` 在本机启动与 `wbot-server` 相同的 Runtime，然后自动打开系统默认浏览器。

```bash
cd /path/to/wbot
make build

export WBOT_MODEL_API_KEY='your-api-key'
export WBOT_WORKSPACE_ROOT='/absolute/path/to/allowed/workspace'
export WBOT_DATA_ROOT="$HOME/.local/share/wbot"

./bin/wbot-desktop
```

它仍然需要当前应用目录中的 `web/dist/` 和 `profiles/default.yaml`。

如果已经在服务器部署了 wbot，也可以把此启动器仅作为“打开远端页面”的快捷入口：

```bash
WBOT_REMOTE_URL='https://wbot.example.com' ./bin/wbot-desktop
```

远程模式只会在浏览器打开该 URL，不会在本地启动 Runtime，也不会让远端服务获得本机文件访问能力。

## 6. 方案 C：Wails 原生窗口

构建本地 Wails v2 窗口：

```bash
cd /path/to/wbot
make desktop-native
```

该目标会添加 Wails 运行所需的 `desktop`、`production` 构建标签；在 macOS 上还会链接 `UniformTypeIdentifiers` framework，并对最终 Mach-O 执行本机 ad-hoc 签名，以兼容新版 macOS SDK 和运行时签名校验。直接执行 `go build -tags wails` 生成的程序不能正常启动。

启动：

```bash
export WBOT_MODEL_API_KEY='your-api-key'
export WBOT_WORKSPACE_ROOT='/absolute/path/to/allowed/workspace'
export WBOT_DATA_ROOT="$HOME/.local/share/wbot"

./bin/wbot-wails
```

当前 Wails 入口是本地一体化模式：Go Agent Core、API、SSE 和 WebView 位于同一应用进程。它目前没有实现 `WBOT_REMOTE_URL` 远程模式；远端访问请使用浏览器或 `wbot-desktop`。

本地 Wails 不监听 HTTP 网络端口，API Handler 只挂载在应用内 WebView，因此该入口会在代码层忽略 `WBOT_AUTH_TOKEN`，无需登录或执行 `unset`。`wbot-server` 和 `wbot-desktop` 的浏览器模式仍按配置启用 Token 认证。模型访问所需的 `WBOT_MODEL_API_KEY` 不受影响，仍然必须设置。

当前 `make desktop-native` 生成的是运行二进制，不是完整的 macOS `.app`、Windows 安装包或 Linux 分发包。运行时仍需保留 `web/dist/` 和 Profile。正式制作各平台安装包时，应再补充 Wails 平台打包配置。

## 7. 方案 D：Linux 单机服务

推荐拓扑：

```text
Browser -> HTTPS Nginx/Caddy -> 127.0.0.1:8080 wbot-server
                                      |-- /var/lib/wbot
                                      `-- /srv/wbot/workspace
```

### 7.1 准备发布目录

先在构建机执行 `make build`，然后把以下内容复制到服务器：

```text
/opt/wbot/
├── bin/wbot-server
├── web/dist/
└── profiles/default.yaml
```

服务器上创建用户和持久目录：

```bash
sudo useradd --system --home /var/lib/wbot --shell /usr/sbin/nologin wbot
sudo install -d -m 750 -o wbot -g wbot /var/lib/wbot
sudo install -d -m 750 -o wbot -g wbot /srv/wbot/workspace
sudo chown -R root:root /opt/wbot
sudo chmod 755 /opt/wbot/bin/wbot-server
```

### 7.2 配置环境文件

生成认证 Token：

```bash
openssl rand -hex 32
```

创建 `/etc/wbot/wbot.env`：

```bash
WBOT_MODEL_API_KEY=replace-with-api-key
WBOT_MODEL_BASE_URL=https://api.deepseek.com
# 不设置 WBOT_ADVISOR_BASE_URL 时自动继承 WBOT_MODEL_BASE_URL
# WBOT_ADVISOR_BASE_URL=https://advisor-api.example.com
WBOT_ADVISOR_MODEL=deepseek-v4-pro
WBOT_AUTH_TOKEN=replace-with-random-token
WBOT_ADDR=127.0.0.1:8080
WBOT_DATA_ROOT=/var/lib/wbot
WBOT_DATABASE_PATH=/var/lib/wbot/wbot.db
WBOT_WORKSPACE_ROOT=/srv/wbot/workspace
WBOT_PROFILE=/opt/wbot/profiles/default.yaml
WBOT_PERMISSION_MODE=approval
WBOT_ALLOW_SHELL=true
```

限制凭据文件权限：

```bash
sudo chown root:wbot /etc/wbot/wbot.env
sudo chmod 640 /etc/wbot/wbot.env
```

### 7.3 配置 systemd

创建 `/etc/systemd/system/wbot.service`：

```ini
[Unit]
Description=wbot Agent Runtime
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=wbot
Group=wbot
WorkingDirectory=/opt/wbot
EnvironmentFile=/etc/wbot/wbot.env
ExecStart=/opt/wbot/bin/wbot-server
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

加载并启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now wbot
sudo systemctl status wbot
sudo journalctl -u wbot -f
```

### 7.4 配置 HTTPS 与 SSE 代理

Nginx 核心配置示例：

```nginx
server {
    listen 443 ssl http2;
    server_name wbot.example.com;

    # ssl_certificate 与 ssl_certificate_key 按实际证书配置

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

        # SSE 事件流需要关闭缓冲并延长读取超时
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
    }
}
```

不要把未启用 `WBOT_AUTH_TOKEN` 和 HTTPS 的服务直接暴露到公网。

## 8. 方案 E：Docker Compose

Docker 部署不要求宿主机安装 Go 或 Node.js，只需要 Docker 与 Compose。

### 8.1 准备环境

```bash
cd /path/to/wbot
mkdir -p workspace

cat > .env <<'EOF'
WBOT_MODEL_API_KEY=replace-with-api-key
WBOT_MODEL_BASE_URL=https://api.deepseek.com
# 留空或不设置时，Advisor 自动使用 WBOT_MODEL_BASE_URL
WBOT_ADVISOR_BASE_URL=
WBOT_ADVISOR_MODEL=deepseek-v4-pro
WBOT_AUTH_TOKEN=replace-with-a-random-long-token
WBOT_PERMISSION_MODE=approval
EOF
```

`workspace/` 是容器内 Agent 可以访问的授权工作区。不要把整个宿主机根目录或用户主目录挂载给容器。

### 8.2 构建和启动

```bash
docker compose up -d --build
docker compose ps
docker compose logs -f wbot
```

访问 <http://127.0.0.1:8080>。当前 Compose 配置：

- 宿主机 `8080` 映射到容器 `8080`；
- `./workspace` 挂载为容器 `/workspace`；
- 具名卷 `wbot-data` 挂载为 `/var/lib/wbot`；
- Profile 位于镜像内 `/app/profiles/default.yaml`。

停止但保留数据：

```bash
docker compose down
```

停止并删除 `wbot-data` 会永久删除数据库、记忆和 Artifact，因此不要在日常升级时执行 `docker compose down -v`。

生产容器仍应放在 HTTPS 反向代理之后。一个 `wbot-data` 卷只能挂载给一个可写 wbot 副本。

## 9. 完整配置参考

### 9.1 Runtime 与安全

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WBOT_ADDR` | `127.0.0.1:8080` | HTTP 监听地址；容器内设置为 `0.0.0.0:8080` |
| `WBOT_DATA_ROOT` | `<启动目录>/.wbot-data` | SQLite、记忆和 Artifact 根目录 |
| `WBOT_DATABASE_PATH` | `<WBOT_DATA_ROOT>/wbot.db` | SQLite 数据库路径 |
| `WBOT_WORKSPACE_ROOT` | 启动目录 | 文件和 Shell 工具的授权根目录 |
| `WBOT_PROFILE` | `<启动目录>/profiles/default.yaml` | Profile 文件路径 |
| `WBOT_PERMISSION_MODE` | `approval` | 仅支持 `approval` 或 `full_access` |
| `WBOT_AUTH_TOKEN` | 空 | 设置后 Server/浏览器 UI 必须认证；本地 Wails 忽略此变量 |
| `WBOT_ALLOW_SHELL` | `true` | 是否允许 Shell 工具 |
| `WBOT_ALLOW_NETWORK` | `true` | 当前为预留策略开关 |
| `WBOT_ALLOW_EXTERNAL_WRITE` | `false` | 当前为预留策略开关 |
| `WBOT_TASK_MAX_PARALLELISM` | `4` | 任务准备节点并行度上限 |
| `WBOT_MAX_CONTEXT_TOKENS` | `60000` | 上下文预算，超出后触发摘要压缩 |
| `WBOT_ADVISOR_MAX_CALLS_PER_TASK` | `3` | 每个任务最多调用 Advisor 的次数 |
| `WBOT_REMOTE_URL` | 空 | 仅供 `wbot-desktop` 打开远端 Web 页面 |

### 9.2 模型配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WBOT_MODEL_API_KEY` | 空 | 默认模型和 Advisor 共用的通用 API Key，运行任务必需 |
| `WBOT_MODEL_BASE_URL` | `https://api.deepseek.com` | 默认模型的 OpenAI 兼容服务地址 |
| `WBOT_DEFAULT_MODEL` | `deepseek-v4-flash` | 默认执行模型名称 |
| `WBOT_DEFAULT_MAX_OUTPUT_TOKENS` | `16000` | 默认模型最大输出 Token |
| `WBOT_MODEL_TIMEOUT_SECONDS` | `120` | 默认模型请求超时秒数 |
| `WBOT_ADVISOR_BASE_URL` | 等于 `WBOT_MODEL_BASE_URL` | Advisor 服务地址 |
| `WBOT_ADVISOR_MODEL` | `deepseek-v4-pro` | Advisor 模型名称 |
| `WBOT_ADVISOR_MAX_OUTPUT_TOKENS` | `32000` | Advisor 最大输出 Token |
| `WBOT_ADVISOR_TIMEOUT_SECONDS` | `180` | Advisor 请求超时秒数 |

如果使用其他 OpenAI Chat Completions 兼容服务，至少需要设置服务地址、模型名称和 `WBOT_MODEL_API_KEY`。`WBOT_MODEL_API_KEY` 是唯一支持的模型密钥变量。

## 10. 权限与审批

`WBOT_PERMISSION_MODE=approval`：

- 文件读取和无副作用操作自动允许；
- 授权工作区内普通文件写入属于 L1，当前策略自动允许；
- Shell 属于 L2，按“任务 + 工具 + 完整参数”精确审批；
- 越出 `WBOT_WORKSPACE_ROOT` 的文件路径直接拒绝；
- 审批后原任务自动恢复。

审批必须点击 Web UI 右侧“待审批”卡片中的“批准/拒绝”，或者调用审批 API。在聊天框输入“允许”或“批准”只会创建一个新任务，不等于审批。

`WBOT_PERMISSION_MODE=full_access` 会跳过 L2 人工审批，但不会关闭工具参数校验、审计记录和操作系统自身权限。Shell 命令本身仍可能访问工作区以外位置，因此只应在隔离、可信环境使用。

## 11. Web UI 使用

1. 打开页面；如果服务设置了 `WBOT_AUTH_TOKEN`，先在登录页输入 Token。
2. 点击“打开默认工作区”。默认路径就是 `WBOT_WORKSPACE_ROOT`。
3. 在工作区卡片中点击“新建会话”。
4. 输入目标，按 `Enter` 或点击“发送”；使用 `Shift + Enter` 换行。
5. 中间区域查看消息和工具结果，右侧查看任务图。
6. 出现待审批操作时，核对工具名称、参数和风险级别，再批准或拒绝。

## 12. HTTP API 示例

准备地址和 Token：

```bash
export WBOT_URL='http://127.0.0.1:8080'
export WBOT_TOKEN='replace-with-your-token'
```

打开授权根目录作为 Workspace：

```bash
curl -H "Authorization: Bearer $WBOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo","path":"/absolute/authorized/workspace","kind":"local"}' \
  "$WBOT_URL/api/v1/workspaces/open"
```

`path` 必须位于 `WBOT_WORKSPACE_ROOT` 内。省略 `path` 时使用 `WBOT_WORKSPACE_ROOT`。

创建 Session 并发送消息：

```bash
curl -H "Authorization: Bearer $WBOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"workspaceId":"ws_xxx","title":"演示"}' \
  "$WBOT_URL/api/v1/sessions"

curl -H "Authorization: Bearer $WBOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"content":"读取 README.md 并生成摘要"}' \
  "$WBOT_URL/api/v1/sessions/session_xxx/messages"
```

查询任务和处理审批：

```bash
curl -H "Authorization: Bearer $WBOT_TOKEN" \
  "$WBOT_URL/api/v1/tasks/task_xxx"

curl -H "Authorization: Bearer $WBOT_TOKEN" \
  "$WBOT_URL/api/v1/approvals?status=pending"

curl -X POST -H "Authorization: Bearer $WBOT_TOKEN" \
  "$WBOT_URL/api/v1/approvals/approval_xxx/approve"
```

Session 事件流使用 SSE：

```bash
curl -N -H "Authorization: Bearer $WBOT_TOKEN" \
  "$WBOT_URL/api/v1/sessions/session_xxx/events"
```

## 13. 数据目录、备份与恢复

默认数据结构：

```text
WBOT_DATA_ROOT/
├── wbot.db
├── wbot.db-wal
├── wbot.db-shm
├── memory/
│   ├── index.yaml
│   ├── user/entries.md
│   ├── project/entries.md
│   ├── episodic/entries.md
│   └── procedural/entries.md
└── artifacts/<hash-prefix>/<sha256>
```

分类目录会在对应类型第一次写入时创建；没有内容的类型可能暂时不存在。数据库升级时会自动执行幂等 Schema Migration；不要通过删除数据库处理字段不兼容问题。

### 13.1 推荐的停机备份

裸机部署：

```bash
sudo systemctl stop wbot
sudo tar -C /var/lib -czf /backup/wbot-$(date +%F-%H%M%S).tar.gz wbot
sudo systemctl start wbot
```

本地部署：先停止 `wbot-server`，再复制整个 `WBOT_DATA_ROOT`，不要只复制 `wbot.db`。

Docker 部署：

```bash
docker compose stop wbot
docker run --rm \
  -v wbot_wbot-data:/data:ro \
  -v "$PWD/backup":/backup \
  alpine sh -c 'tar -C /data -czf /backup/wbot-data.tar.gz .'
docker compose start wbot
```

Compose 项目名会影响 Volume 名称；先用 `docker volume ls` 确认实际名称。

恢复时停止服务，把完整数据目录恢复到原位置，确认运行用户具有读写权限，然后再启动。生产升级前应备份并定期演练恢复。

## 14. 升级与回滚

### 14.1 裸机升级

1. 备份 `WBOT_DATA_ROOT`；
2. 在新版本源码执行 `make test && make build`；
3. 同时替换 `bin/wbot-server`、`web/dist/` 和需要更新的 Profile；
4. 重启服务并检查日志与 `/healthz`；
5. 验证打开工作区、新建会话、发送消息和审批流程。

```bash
sudo systemctl restart wbot
curl https://wbot.example.com/healthz
sudo journalctl -u wbot -n 100 --no-pager
```

### 14.2 Docker 升级

```bash
docker compose build --pull
docker compose up -d
docker compose logs --tail=100 wbot
```

不要在升级时添加 `-v`。如果需要回滚应用版本，应恢复旧镜像；如果新版本已经迁移数据库且旧版本不兼容，还需要恢复升级前的数据备份。

## 15. 常见故障

| 现象 | 原因与处理 |
|---|---|
| `WBOT_MODEL_API_KEY is not configured` | 设置模型 API Key 后重启服务 |
| `address already in use` | 已有进程占用 `WBOT_ADDR`；停止旧实例或更换端口 |
| 页面只显示 API 运行提示 | 缺少 `web/dist/`，在应用根目录执行 `make build` |
| Profile 文件找不到 | 从应用根目录启动，或把 `WBOT_PROFILE` 设置为绝对路径 |
| `path escapes workspace` | Workspace 或文件路径超出 `WBOT_WORKSPACE_ROOT` |
| 任务停在 `waiting_approval` | 在右侧审批面板处理；聊天输入“批准”不生效 |
| `401 Unauthorized` | 在 Web 登录页输入 `WBOT_AUTH_TOKEN`，或为 API 添加 Bearer Header |
| 模型返回 400/模型不存在 | 检查 Base URL、模型名称和服务端协议兼容性 |
| SQLite busy/locked | 确认没有第二个进程或容器写同一个数据库 |
| Docker 工作区不可写 | 检查宿主机 `workspace/` 的所有者、权限及容器 UID 10001 |
| SSE 状态不更新 | 关闭反向代理缓冲并增加 `proxy_read_timeout` |

## 16. 生产安全检查表

- 使用强随机 `WBOT_AUTH_TOKEN`，并通过 Secret 或权限受限的环境文件注入。
- 使用 HTTPS，不直接公开 `0.0.0.0:8080`。
- 使用独立、非 root 的系统用户或容器用户运行。
- 将 `WBOT_WORKSPACE_ROOT` 和容器挂载范围缩到实际需要的目录。
- 生产环境默认使用 `approval`，谨慎启用 `full_access`。
- 不把 API Key、Token 或私钥写入 Profile、镜像、源码和日志。
- 每个实例使用独立的 `WBOT_DATA_ROOT` 和 SQLite 数据库。
- 定期备份数据库、记忆和 Artifact，并实际演练恢复。
- 升级后验证健康检查、Web 登录、消息、工具、审批和 SSE 事件流。
