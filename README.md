# wbot

wbot 是一个可恢复、可审批、可审计的自主 Agent Runtime。默认模型负责规划和工具执行，高能力 Advisor 按需提供复杂推理建议。

## 快速开始

```bash
make build
export WBOT_MODEL_API_KEY='your-api-key'
export WBOT_MODEL_BASE_URL='https://api.deepseek.com'
export WBOT_ADVISOR_MODEL='deepseek-v4-pro'
# 可选：不设置时 Advisor 自动使用 WBOT_MODEL_BASE_URL
# export WBOT_ADVISOR_BASE_URL='https://advisor-api.example.com'
export WBOT_WORKSPACE_ROOT='/absolute/path/to/workspace'
./bin/wbot-server
```

打开 <http://127.0.0.1:8080>。

完整说明：

- [技术设计](TECH.md)
- [部署、配置及使用说明](docs/DEPLOYMENT.md)
- [V1 验收记录](docs/V1_ACCEPTANCE.md)

## 验证

```bash
make test
make build
```
