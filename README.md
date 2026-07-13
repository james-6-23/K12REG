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
├── scripts/sentinel_vm/   # Node：turnstile.dx
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

```bash
./k12reg serve -h
./k12reg -h
```

环境变量：`WEB_PASSWORD` `PORT` `K12_DATA_DIR` `K12_STATIC_DIR` `K12_SENTINEL_VM` `WEB_SESSION_DAYS`
