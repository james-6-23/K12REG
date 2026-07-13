# chatgpt-register-sub2api

ChatGPT 账号自动注册 → K12 母号加入 → Sub2API JSON 导出，一条龙自动化工具。

从 [chatgpt2api](https://github.com/basketikun/chatgpt2api) 提取注册机核心逻辑，融合 workspace 加入和 sub2api 格式转换。

当前仅支持 **协议注册**（curl_cffi + Sentinel）。

支持两种用法：命令行（`chatgpt-register`）或 **Docker + Web 控制台**（见下方「Docker 部署 + Web 界面」）。

## Docker 部署 + Web 界面

单容器打包了 Python 流水线 + Node（Sentinel JSVMP）+ FastAPI Web 控制台。
控制台可在浏览器里：在线编辑 `config.toml`、上传邮箱池 / 代理 / session 文件、
启动/停止流水线、看实时日志、查看账号结果并下载 `access_token.txt`。

```bash
# 1. 改密码（docker-compose.yml 里的 WEB_PASSWORD）
# 2. 构建并启动
docker compose up -d --build

# 3. 浏览器打开 http://<服务器IP>:8000 ，用 WEB_PASSWORD 登录
```

所有运行数据都在挂载卷 `./data`（宿主）↔ `/data`（容器）里持久化：
`config.toml`、`outlook.txt` / `hotmail.txt`、`proxies.txt`、`session.json` /
`hotsession.json`，以及输出 `registered_accounts.json` / `access_token.txt`。
首次启动 `./data` 为空，登录后在「配置」页点「载入默认模板」→ 改好 → 保存，
再到「数据文件」页上传邮箱池 / 代理 / session，最后在「运行」页点启动。

环境变量：

| 变量 | 默认 | 说明 |
|------|------|------|
| `WEB_PASSWORD` | `admin` | Web 控制台登录密码（**务必修改**） |
| `PORT` | `8000` | 监听端口 |
| `K12_DATA_DIR` | `/data` | 数据/配置目录（挂载卷） |

不用 compose 也可以直接跑：

```bash
docker build -t k12reg .
docker run -d -p 8000:8000 -e WEB_PASSWORD=你的密码 -v $(pwd)/data:/data k12reg
```

本地不装 Docker 直接跑 Web 控制台：

```bash
pip install -e ".[web]"
K12_DATA_DIR=. WEB_PASSWORD=你的密码 uvicorn webapp.server:app --host 0.0.0.0 --port 8000
```

## 安装

```bash
git clone <this-repo> && cd chatgpt-register-sub2api
python3 -m venv .venv
source .venv/bin/activate
pip install -e .
```

依赖：

- `curl-cffi` — TLS 指纹伪装（协议注册 / join / session 换 AT）

配置文件用 TOML，解析走 Python 3.11+ 标准库 `tomllib`。

## 快速开始

```bash
# 1. 安装
pip install -e .

# 2. 生成配置文件（若还没有 config.toml）
chatgpt-register init

# 3. 编辑 config.toml，填入：
#    - 代理（proxies.txt 或单节点 url）
#    - K12 workspace ID
#    - Outlook 邮箱池：mailboxes_file = "outlook.txt"
#    - 注册数量 / 并发 threads

# 4. 一条龙运行（无需子命令，直接读 config.toml）
chatgpt-register

# 仅注册一步：
chatgpt-register register -n 1
```

## 命令

无子命令时直接按 `config.toml` 跑完整流水线。也可单独执行某一步：

| 命令 | 说明 |
|------|------|
| （无） | 按 config.toml 跑完整流水线 |
| `init` | 生成默认 config.toml |
| `register` | 只注册 ChatGPT 账号 |
| `join-workspace` | 只执行 workspace 加入 |
| `login-team` | 只执行重新登录（team 空间） |
| `export` | 只导出 sub2api JSON |
| `import-at` | 把 access token 推送到外部账号池 API |

## 完整流水线

```
[1] 协议注册账号（curl_cffi + Sentinel）
      │  调 auth API → 邮箱 OTP
      │  产物: access_token + refresh_token + id_token
      ▼
[2] 加入 K12 母号 workspace (子号 POST /invites/request)
      │
      ▼
[3] ⚡ 母号(管理员)批准加入请求
      │  用 session.json 的母号 token,
      │  按子号邮箱逐个批准: PATCH /invites/{id}
      ▼
[4] 刷新 token + check API (拿真实 plan_type / account_id)
      │  有 refresh_token → OAuth refresh
      │  否则有 session_token → 再打 /api/auth/session
      ▼
[5] 输出 access_token.txt → 导入账号池 admin API
```

## 配置文件 (config.toml)

```toml
[[mail.providers]]
type = "outlook_token"
enable = true
mode = "graph"
mailboxes_file = "outlook.txt"
alias_count = 5

[proxy]
url = ""
proxies_file = "proxies.txt"
default_protocol = "socks5"

[registration]
mode = "protocol"
threads = 3
total = 1

[workspace]
enabled = true
ids = ["your-k12-workspace-uuid"]

[import_api]
enabled = false
url = "http://127.0.0.1:2003"
admin_key = "<admin_secret>"
```

## Outlook 邮箱池格式

每行一个邮箱，4 个字段用 `----` 分隔：

```
email----password----client_id----refresh_token
```

推荐把账号池放到项目根目录的 `outlook.txt` 文件中，然后在 config.toml 配置：

```toml
mailboxes_file = "outlook.txt"
```

代码会自动忽略 `outlook.txt` 里的注释行。

从 Microsoft Azure 应用注册获取 `client_id` 和 `refresh_token`。

## 输出文件

- `registered_accounts.json` — 注册成功的账号信息（含 token）
- `sub2api_bundle.json` — Sub2API 格式的账号 bundle

## 注意事项

- 建议使用代理，同一 IP 注册超过 3 个号容易触发风控
- curl_cffi 需要 TLS 指纹伪装，这是绕过 OpenAI 反爬的关键
- 协议模式依赖 Sentinel / 过盾服务，失败率可能偏高
- Team 空间重新登录步骤需要实际 OpenAI 环境调试确认 workspace 选择 API

## 致谢

感谢 [LINUX DO](https://linux.do/) 社区的交流与支持。
