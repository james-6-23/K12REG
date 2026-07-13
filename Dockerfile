# syntax=docker/dockerfile:1

# ── K12REG: ChatGPT 注册流水线 + Web 控制台 ──────────────────────────
# 单容器：Python 流水线 + Node(Sentinel JSVMP) + FastAPI Web UI
FROM python:3.12-slim

# Node 供 Sentinel turnstile.dx JSVMP 执行（缺失时代码会退回纯 Python）
# curl_cffi 需要 libssl / ca-certificates
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        nodejs \
        ca-certificates \
        tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 先装依赖以利用层缓存
COPY pyproject.toml README.md ./
RUN pip install --no-cache-dir \
        "curl-cffi>=0.8.0" "rich>=13.0.0" \
        "fastapi>=0.110.0" "uvicorn[standard]>=0.29.0" "python-multipart>=0.0.9"

# 再拷贝源码
COPY . .

# 数据目录（挂载卷用）：config.toml / outlook.txt / proxies.txt / session.json / 输出
ENV K12_DATA_DIR=/data \
    WEB_PASSWORD=admin \
    PORT=8000 \
    PYTHONUNBUFFERED=1 \
    NO_COLOR=1
RUN mkdir -p /data

EXPOSE 8000

# tini 作 PID 1，正确转发信号（流水线子进程优雅停止）
ENTRYPOINT ["tini", "--"]
CMD ["sh", "-c", "uvicorn webapp.server:app --host 0.0.0.0 --port ${PORT}"]
