# K12REG

ChatGPT 协议注册 → K12 母号加入 → 导出 / 导入。

Go 后端 + Vue 前端 monorepo（标准布局）。

```
K12REG/
├── cmd/server/            # 进程入口（serve | CLI）
├── internal/              # 后端私有包
│   ├── web/               # HTTP API / SSE
│   ├── pipeline/          # 出号编排
│   ├── register/          # 协议注册
│   └── …
├── frontend/              # 前端源码（Vite）
│   └── dist/              # 构建产物（gitignore，Go 托管）
├── scripts/
│   ├── sentinel_vm/       # Node：turnstile.dx
│   └── codex_agent.py     # 参考：Python 版 Codex Agent Identity
├── internal/codexagent/   # Go：Codex CLI agent_identity 注册
├── data/                  # 运行时数据（gitignore）
├── go.mod
├── Makefile
├── Dockerfile
└── docker-compose.yml
```

## 快速开始

```bash
# 依赖：Go 1.26+、Node.js

make build-ui          # frontend → frontend/dist
make build             # → ./k12reg

# Web 控制台
make serve             # http://127.0.0.1:8000  密码 admin

# CLI 出号
make run COUNT=1

# Docker
make docker            # http://IP:8989
```

## 开发

```bash
# API
go run ./cmd/server serve -data ./data -static ./frontend/dist -password admin -addr :8000

# UI 热更新
cd frontend && npm run dev
```

## 使用

1. 数据放在 `data/`：`settings.json`、`hotmail.txt`、`proxies.txt`、`hotsession.json`  
2. Web 设置页改配置 → 运行页启动  
3. **子号邮箱域须与母号同域**（如均为 `@hotmail.com`）  

## 命令

| 命令 | 说明 |
|------|------|
| `k12reg serve` | Web 控制台 + 进程内流水线 |
| `k12reg` | CLI 出号 |
| `k12reg codex-agent` | 从 accessToken 注册 Codex Agent Identity → `auth.json` |

```bash
./k12reg serve -h
./k12reg -h
./k12reg codex-agent -h

# 单 token
./k12reg codex-agent --token "eyJ..." -data ./data

# 批量：access_token.txt / 账号库
./k12reg codex-agent --from-access-token-file -data ./data
./k12reg codex-agent --from-accounts -data ./data
```

设置页开启 **Codex Agent Identity** 后，流水线在写出 live AT 时会自动注册并写入 `data/codex_auth/<email>.json`。  
结果页也可手动批量生成。Web：`POST /api/codex-agent`。

### 导入到 codex2api（Agent Identity）

见 [AGENT_IDENTITY_IMPORT.md](./AGENT_IDENTITY_IMPORT.md)。

| 导入 API 模式 | 网关路径 | 载荷 |
|---|---|---|
| `agent_identity`（推荐） | `POST /api/admin/accounts/codex/agent-identity` / `.../import` | auth.json（私钥 + runtime_id） |
| `at`（旧） | `POST /api/admin/accounts/at` | access_token |

1. **设置 → 导入 API**：URL 填网关根地址（如 `http://127.0.0.1:2004`），`admin_key`，模式选 **Agent ID**  
2. 出号流水线：生成 agent 后自动单条导入  
3. 结果页：**推送到 codex2api** 批量 `.../agent-identity/import`（每批 ≤200）  
4. 或 `POST /api/codex-agent/import`  

环境变量：`WEB_PASSWORD` `PORT` `K12_DATA_DIR` `K12_STATIC_DIR` `K12_SENTINEL_VM` `WEB_SESSION_DAYS`
