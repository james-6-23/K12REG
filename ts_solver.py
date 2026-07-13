#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""分布式 Turnstile solver worker。

从服务器 Redis 队列拉任务 → 用本地真 Chromium(cloakbrowser) + 本地住宅代理产
Turnstile token → 回传 Redis。多台机器可各跑多个，水平扩展产能。

每台 solver 用自己的住宅代理（--proxy），不依赖中心出口。服务器只做队列中转。

协议:
  ts:tasks           list  注册机 LPUSH 任务 json, solver BRPOP 领
  ts:result:<id>     key   solver 写 {"token":...} 或 {"error":...}, TTL 120s
  ts:stats:*         计数  产出/失败统计

用法:
  python3 ts_solver.py --redis <redis-host> --redis-pass XXX \
      --proxy socks5://127.0.0.1:7898 --browsers 3 --pages 6
"""
import argparse
import json
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
ENTRY = ROOT / "xai_http_flow.py"

try:
    import redis
except ImportError:
    print("需要 redis-py: pip install redis"); sys.exit(1)


# ── 内置配置（分发给贡献者，无需自己填服务端）──────────────────
# 公开仓库不内置真实密钥；用环境变量或命令行参数传入。
DEFAULT_API = os.environ.get("TS_API", "")
DEFAULT_API_KEY = os.environ.get("TS_API_KEY", "")
XAI_SITEKEY = "0x4AAAAAAAhr9JGVDZbrZOo0"

BANNER = r"""
  ╔══════════════════════════════════════════════════════════╗
  ║        grok/xAI Turnstile Solver — 贡献者节点              ║
  ║   用你的住宅代理产 Turnstile token，为注册池贡献算力        ║
  ╚══════════════════════════════════════════════════════════╝
"""


def _fmt(sec):
    sec = int(sec)
    return "%02d:%02d:%02d" % (sec // 3600, (sec % 3600) // 60, sec % 60)


def selfcheck(proxy):
    """启动自检：用该代理真跑一次 Turnstile 捕获，能出 token 才算合格。"""
    print("\n  正在验证代理能否通过 Turnstile 打码（首次会下载浏览器内核，请稍候）…", flush=True)
    try:
        import xai_http_flow as flow
    except Exception as exc:
        print("  ✗ 缺少依赖: %s" % exc); return False
    try:
        tok = flow.capture_turnstile_any(
            sitekey=XAI_SITEKEY, page_url=flow.SIGNUP_URL,
            proxy=proxy, timeout=100, headless=True, engine="cloak",
            log_callback=lambda m: None)
        if tok and len(tok) >= 80:
            print("  ✅ 打码测试通过！token 长度 %d，代理可用。\n" % len(tok))
            return True
        print("  ✗ 未能产出 token。"); return False
    except Exception as exc:
        print("  ✗ 打码失败: %s" % str(exc)[:120]); return False


def _ask_proxy():
    """交互询问住宅代理地址。"""
    print(BANNER)
    print("  你的机器需要一个【住宅/家宽】代理来过 Turnstile（机房 IP 过不了）。")
    print("  格式示例：")
    print("    socks5://user:pass@host:port")
    print("    http://user:pass@host:port")
    print("    none                （本机直连，仅当你本地 IP 是住宅时）")
    while True:
        p = input("\n  请输入你的代理地址: ").strip()
        if p:
            return p
        print("  不能为空，请重新输入。")


def main():
    interactive = len(sys.argv) == 1  # 无参数（双击运行）→ 交互向导
    ap = argparse.ArgumentParser(description="分布式 Turnstile solver worker（贡献者节点）")
    ap.add_argument("--api", default="", help="HTTPS API 门面地址（或用环境变量 TS_API）")
    ap.add_argument("--api-key", default="", help="API 门面的 X-API-Key（或用环境变量 TS_API_KEY）")
    ap.add_argument("--redis", default="", help="Redis 直连地址（高级，一般不用）")
    ap.add_argument("--redis-port", type=int, default=6379)
    ap.add_argument("--redis-pass", default="", help="Redis 密码")
    ap.add_argument("--proxy", default="",
                    help="住宅代理（过 Turnstile 用；none=直连）。不填则启动时交互询问")
    ap.add_argument("--no-check", action="store_true", help="跳过启动打码自检（不建议）")
    ap.add_argument("--browsers", type=int, default=3, help="并行浏览器 solver 数（甜点 3）")
    ap.add_argument("--pages", type=int, default=6, help="每浏览器并行页数")
    ap.add_argument("--pool-size", type=int, default=8,
                    help="本地预蓄 token 池水位（无任务时也蓄着，来任务秒回）")
    args = ap.parse_args()

    # 默认连内置服务端（贡献者无需自己填）
    if not args.api and not args.redis:
        args.api = DEFAULT_API
        args.api_key = args.api_key or DEFAULT_API_KEY
    if args.api and not args.api_key:
        args.api_key = DEFAULT_API_KEY

    # 交互向导：无 --proxy 时询问代理地址
    if not args.proxy:
        args.proxy = _ask_proxy()

    proxy = "" if args.proxy in ("none", "") else args.proxy

    # 启动自检：验证该代理能真正打码，不合格直接退出（避免白占队列）
    if not args.no_check:
        if not selfcheck(proxy):
            print("\n  代理无法通过 Turnstile 打码。请换一个【住宅/家宽】代理再试。")
            print("  （机房 IP、云主机 IP、大部分 VPN 都过不了）")
            if interactive:
                input("\n  按回车键退出…")
            sys.exit(1)

    import platform
    worker_id = "%s-%d" % (platform.node()[:12] or "node", os.getpid())

    # 两种后端：API 门面（走 HTTPS/Cloudflare）或直连 Redis。统一成 pull_task/deliver。
    if args.api:
        import urllib.request
        base = args.api.rstrip("/")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        _UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
               "(KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")

        def _api(path, data=None, retries=3):
            headers = {"X-API-Key": args.api_key, "Content-Type": "application/json",
                       "User-Agent": _UA}
            body = json.dumps(data).encode() if data is not None else None
            last = None
            for attempt in range(retries):
                try:
                    req = urllib.request.Request(base + path, data=body, headers=headers,
                                                 method="POST" if data is not None else "GET")
                    with opener.open(req, timeout=25) as resp:
                        if resp.status == 204:
                            return None
                        return json.loads(resp.read().decode() or "{}")
                except Exception as exc:
                    last = exc
                    if attempt < retries - 1:
                        time.sleep(1.5)
            raise last

        # 启动连通性检查（多试几次，容忍 CF 偶发 TLS 抖动）
        ok = False
        for _ in range(5):
            try:
                _api("/stats"); ok = True; break
            except Exception as exc:
                _last = exc; time.sleep(2)
        if not ok:
            print("✗ 连不上 API 门面 %s: %s" % (args.api, _last))
            if interactive:
                input("按回车退出…")
            sys.exit(1)

        def pull_task():
            try:
                return _api("/pull")
            except Exception:
                return None

        def deliver(tid, token="", error=""):
            payload = {"id": tid, "worker": worker_id}
            payload["token" if token else "error"] = token or (error or "solver failed")
            try:
                _api("/deliver", payload)
            except Exception:
                pass

        def heartbeat(payload):
            # 用独立 opener，避免和主循环的 /pull 阻塞请求共用 urllib 连接导致互卡。
            # 带重试：CF 偶发 TLS 握手抖动时，单次会失败，重试才能稳定上报设备状态。
            hop = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            body = json.dumps(payload).encode()
            for _ in range(3):
                try:
                    req = urllib.request.Request(base + "/heartbeat", data=body,
                        headers={"X-API-Key": args.api_key, "Content-Type": "application/json",
                                 "User-Agent": _UA}, method="POST")
                    with hop.open(req, timeout=20) as resp:
                        d = json.loads(resp.read().decode() or "{}")
                    return d.get("want_browsers")
                except Exception:
                    time.sleep(1.5)
            return None
        backend_desc = "API %s" % args.api
    else:
        if not args.redis:
            print("✗ 需要 --api 或 --redis 之一"); sys.exit(1)
        r = redis.Redis(host=args.redis, port=args.redis_port,
                        password=args.redis_pass or None, socket_timeout=15,
                        socket_keepalive=True, health_check_interval=30)
        try:
            r.ping()
        except Exception as exc:
            print("✗ 连不上 Redis %s: %s" % (args.redis, exc)); sys.exit(1)

        def pull_task():
            item = r.brpop("ts:tasks", timeout=5)
            if not item:
                return None
            try:
                return json.loads(item[1])
            except Exception:
                return None

        def deliver(tid, token="", error=""):
            if token:
                r.setex("ts:result:%s" % tid, 120, json.dumps({"token": token, "worker": worker_id}))
                try: r.incr("ts:stats:served")
                except Exception: pass
            else:
                r.setex("ts:result:%s" % tid, 120, json.dumps({"error": error or "solver timeout", "worker": worker_id}))
                try: r.incr("ts:stats:fail")
                except Exception: pass

        def heartbeat(payload):
            try:
                wid = payload["id"]
                r.hset("ts:device:%s" % wid, mapping={k: payload[k] for k in
                       ("proxy", "browsers", "pages", "pool", "served", "fail", "uptime")})
                r.expire("ts:device:%s" % wid, 30)
                want = r.get("ts:device:%s:want" % wid)
                return int(want) if want else None
            except Exception:
                return None
        backend_desc = "Redis %s:%d" % (args.redis, args.redis_port)

    print("=" * 60)
    print("  分布式 Turnstile solver | worker=%s" % worker_id)
    print("  队列: %s  代理: %s" % (backend_desc, proxy or "直连"))
    print("  本地池: %d 浏览器 × %d 页 | 预蓄水位 %d" % (args.browsers, args.pages, args.pool_size))
    print("=" * 60)

    # 本地 token 池目录（复用现有 turnstile-pool 生产者，产到本地文件池）
    import tempfile
    pool_dir = tempfile.mkdtemp(prefix="ts_solver_pool_")
    stop_file = os.path.join(pool_dir, "_stop")

    # 生产者启动命令：打包成 exe 后自我再调用（sys.executable=exe），开发时调 xai_http_flow.py
    if getattr(sys, "frozen", False):
        base_cmd = [sys.executable]           # 冻结后 exe 自派发 turnstile-pool
    else:
        base_cmd = [sys.executable, str(ENTRY)]
    popen_kw = {}
    if os.name == "posix":
        popen_kw["start_new_session"] = True  # 独立进程组，便于整组清理
    else:
        popen_kw["creationflags"] = 0x00000200  # CREATE_NEW_PROCESS_GROUP (Windows)

    procs = []
    procs_lock = threading.Lock()
    _spawn_seq = [0]

    def spawn_one():
        i = _spawn_seq[0]; _spawn_seq[0] += 1
        pcmd = base_cmd + ["turnstile-pool",
                "--pool-dir", pool_dir, "--target-size", str(args.pool_size),
                "--pages", str(args.pages), "--stop-file", stop_file]
        if proxy:
            pcmd += ["--proxy", proxy]
        return subprocess.Popen(pcmd,
                     stdout=open(os.path.join(pool_dir, "prod_%d.log" % i), "w"),
                     stderr=subprocess.STDOUT, **popen_kw)

    def _kill_one(p):
        try:
            if os.name == "posix":
                os.killpg(os.getpgid(p.pid), signal.SIGKILL)
            else:
                p.kill()
        except Exception:
            try: p.kill()
            except Exception: pass

    # 起初始生产者（各自独占主线程跑 cloakbrowser），错峰
    for _ in range(args.browsers):
        with procs_lock:
            procs.append(spawn_one())
        time.sleep(1.5)
    print("已起 %d 个本地生产者，预热中…" % args.browsers)

    import xai_http_flow as flow

    stats = {"served": 0, "fail": 0, "started": time.time()}
    stopping = threading.Event()

    def _shutdown(*_a):
        stopping.set()
        try:
            Path(stop_file).write_text("stop")
        except Exception:
            pass
        with procs_lock:
            for p in procs:
                _kill_one(p)
        print("\nsolver 已停止，生产者与浏览器已清理。")
        os._exit(0)

    signal.signal(signal.SIGINT, _shutdown)
    try:
        signal.signal(signal.SIGTERM, _shutdown)   # Windows 下可能不支持
    except Exception:
        pass

    # 状态刷新线程
    def _ui():
        while not stopping.wait(3):
            live = flow._count_pooled_tokens(pool_dir)
            with procs_lock:
                alive = sum(1 for p in procs if p.poll() is None)
                total = len(procs)
            up = _fmt(time.time() - stats["started"])
            sys.stdout.write("\r  池 %d/%d  已交付 %d  失败 %d  生产者 %d/%d  运行 %s\033[K"
                             % (live, args.pool_size, stats["served"], stats["fail"],
                                alive, total, up))
            sys.stdout.flush()
    threading.Thread(target=_ui, daemon=True).start()

    # 心跳 + 远程扩缩线程：每 8s 上报状态，读管理面板下发的期望浏览器数并调整
    proxy_show = proxy or "直连"
    if "@" in proxy_show:  # 脱敏代理里的账号密码
        proxy_show = proxy_show.split("://")[0] + "://***@" + proxy_show.split("@")[-1]

    def _heartbeat():
        while not stopping.wait(8):
          try:
            with procs_lock:
                procs[:] = [p for p in procs if p.poll() is None]
                cur = len(procs)
            want = heartbeat({
                "id": worker_id, "proxy": proxy_show, "browsers": cur,
                "pages": args.pages, "pool": flow._count_pooled_tokens(pool_dir),
                "served": stats["served"], "fail": stats["fail"],
                "uptime": int(time.time() - stats["started"]),
            })
            if want is None:
                continue
            want = max(0, min(12, int(want)))
            with procs_lock:
                cur = len(procs)
                if want > cur:
                    for _ in range(want - cur):
                        procs.append(spawn_one())
                elif want < cur:
                    for _ in range(cur - want):
                        if procs:
                            _kill_one(procs.pop())
          except Exception:
            pass   # 心跳/扩缩出错不影响主循环产 token
    threading.Thread(target=_heartbeat, daemon=True).start()

    # 主循环：拉任务 → 从本地池取 token → 回传（后端 API 或 Redis 均已抽象）
    while not stopping.is_set():
        try:
            task = pull_task()
        except Exception:
            time.sleep(2); continue
        if not task:
            continue
        tid = task.get("id", "")
        if not tid:
            continue
        # 从本地池取一个 token（生产者持续在产）；池空则等一会
        token = ""
        deadline = time.time() + int(task.get("timeout", 120))
        while time.time() < deadline and not stopping.is_set():
            token = flow._take_pooled_token(pool_dir)
            if token:
                break
            time.sleep(1)
        if token:
            deliver(tid, token=token)
            stats["served"] += 1
        else:
            deliver(tid, error="solver timeout")
            stats["fail"] += 1


if __name__ == "__main__":
    # 打包成 exe 后，生产者子进程通过 exe 自身再拉起：识别 turnstile-pool 子命令派发。
    if len(sys.argv) > 1 and sys.argv[1] == "turnstile-pool":
        import xai_http_flow
        sys.exit(xai_http_flow.main())
    main()
