"use strict";

// ── tiny helpers ────────────────────────────────────────────────────
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

async function api(path, opts = {}) {
  const res = await fetch(path, { credentials: "same-origin", ...opts });
  if (res.status === 401) {
    showLogin();
    throw new Error("未登录");
  }
  return res;
}
async function apiJSON(path, opts = {}) {
  const res = await api(path, opts);
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).detail || msg; } catch (e) {}
    throw new Error(msg);
  }
  const ct = res.headers.get("content-type") || "";
  return ct.includes("application/json") ? res.json() : res.text();
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

// ── auth ────────────────────────────────────────────────────────────
function showLogin() {
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  if (logSource) { logSource.close(); logSource = null; }
}
function showApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  bootApp();
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#login-error").textContent = "";
  const fd = new FormData();
  fd.append("password", $("#password").value);
  const res = await fetch("/api/login", { method: "POST", body: fd, credentials: "same-origin" });
  if (res.ok) { $("#password").value = ""; showApp(); }
  else {
    let msg = "登录失败";
    try { msg = (await res.json()).detail || msg; } catch (e) {}
    $("#login-error").textContent = msg;
  }
});

$("#logout").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST", credentials: "same-origin" });
  showLogin();
});

// ── tabs ────────────────────────────────────────────────────────────
$$(".tabs button").forEach((btn) => {
  btn.addEventListener("click", () => {
    $$(".tabs button").forEach((b) => b.classList.remove("active"));
    $$(".tab").forEach((t) => t.classList.remove("active"));
    btn.classList.add("active");
    $("#tab-" + btn.dataset.tab).classList.add("active");
    if (btn.dataset.tab === "config") loadConfig();
    if (btn.dataset.tab === "files") loadFiles();
    if (btn.dataset.tab === "results") loadAccounts();
  });
});

// ── run / logs ──────────────────────────────────────────────────────
let logSource = null;

function classifyLine(line) {
  const s = line.toLowerCase();
  if (line.startsWith("▶") || line.startsWith("■") || line.startsWith("⏹")) return "l-sys";
  if (/\b(fail|error|✗|401|403|429)\b/.test(s)) return "l-fail";
  if (/\b(warn|warning)\b/.test(s)) return "l-warn";
  if (/\b(ok|success|k12|imp|✓|registered|joined)\b/.test(s)) return "l-ok";
  return "l-info";
}

function appendLog(line) {
  const el = $("#log");
  const near = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  const span = document.createElement("span");
  span.className = classifyLine(line);
  span.textContent = line + "\n";
  el.appendChild(span);
  if (near) el.scrollTop = el.scrollHeight;
}

function startLogStream() {
  if (logSource) return;
  logSource = new EventSource("/api/run/logs");
  logSource.onmessage = (ev) => {
    try { appendLog(JSON.parse(ev.data)); } catch (e) {}
  };
  logSource.onerror = () => { /* browser auto-reconnects */ };
}

async function refreshRunStatus() {
  try {
    const st = await apiJSON("/api/run/status");
    const badge = $("#run-badge");
    if (st.running) {
      badge.className = "badge running";
      badge.textContent = "运行中";
      $("#btn-start").disabled = true;
      $("#btn-stop").disabled = false;
      const el = st.elapsed ? ` · ${Math.round(st.elapsed)}s` : "";
      $("#run-status").textContent = `pid ${st.pid}${el}`;
    } else {
      badge.className = st.exit_code === null ? "badge idle" : "badge done";
      badge.textContent = st.exit_code === null ? "空闲" : `已结束 (exit ${st.exit_code})`;
      $("#btn-start").disabled = false;
      $("#btn-stop").disabled = true;
      $("#run-status").textContent = "";
    }
  } catch (e) {}
}

$("#btn-start").addEventListener("click", async () => {
  const v = $("#run-count").value.trim();
  const body = { count: v ? parseInt(v, 10) : null };
  try {
    await apiJSON("/api/run/start", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    refreshRunStatus();
  } catch (e) { alert("启动失败: " + e.message); }
});

$("#btn-stop").addEventListener("click", async () => {
  try {
    await apiJSON("/api/run/stop", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ force: false }),
    });
    refreshRunStatus();
  } catch (e) { alert("停止失败: " + e.message); }
});

$("#btn-clear").addEventListener("click", () => { $("#log").innerHTML = ""; });

// ── config ──────────────────────────────────────────────────────────
async function loadConfig() {
  try {
    const text = await apiJSON("/api/config");
    $("#config-text").value = text;
    $("#cfg-status").textContent = "";
  } catch (e) { $("#cfg-status").textContent = e.message; }
}
$("#cfg-reload").addEventListener("click", loadConfig);
$("#cfg-default").addEventListener("click", async () => {
  const text = await apiJSON("/api/config/default", { method: "POST" });
  $("#config-text").value = text;
  $("#cfg-status").textContent = "已载入默认模板（尚未保存）";
});
$("#cfg-save").addEventListener("click", async () => {
  try {
    await apiJSON("/api/config", {
      method: "PUT",
      headers: { "content-type": "text/plain" },
      body: $("#config-text").value,
    });
    $("#cfg-status").textContent = "✓ 已保存";
  } catch (e) { $("#cfg-status").textContent = "✗ " + e.message; }
});

// ── files ───────────────────────────────────────────────────────────
let currentFile = null;
async function loadFiles() {
  const { files } = await apiJSON("/api/files");
  const tb = $("#files-table tbody");
  tb.innerHTML = "";
  files.forEach((f) => {
    const tr = document.createElement("tr");
    const editLink = f.editable
      ? `<span class="link" data-edit="${f.name}">编辑</span>`
      : `<span class="muted">二进制</span>`;
    tr.innerHTML = `
      <td>${f.name}</td>
      <td class="muted">${fmtSize(f.size)}</td>
      <td class="actions">
        ${editLink}
        <span class="link" data-dl="${f.name}">下载</span>
        <span class="link" data-del="${f.name}">删除</span>
      </td>`;
    tb.appendChild(tr);
  });
}

$("#files-table").addEventListener("click", async (e) => {
  const t = e.target;
  if (t.dataset.edit) openFile(t.dataset.edit);
  else if (t.dataset.dl) window.open("/api/download?name=" + encodeURIComponent(t.dataset.dl), "_blank");
  else if (t.dataset.del) {
    if (!confirm("删除 " + t.dataset.del + " ?")) return;
    await apiJSON("/api/file?name=" + encodeURIComponent(t.dataset.del), { method: "DELETE" });
    if (currentFile === t.dataset.del) { currentFile = null; $("#file-text").value = ""; $("#edit-name").textContent = "未选择文件"; $("#file-save").disabled = true; }
    loadFiles();
  }
});

async function openFile(name) {
  const text = await apiJSON("/api/file?name=" + encodeURIComponent(name));
  currentFile = name;
  $("#edit-name").textContent = name;
  $("#file-text").value = text;
  $("#file-save").disabled = false;
}
$("#file-save").addEventListener("click", async () => {
  if (!currentFile) return;
  await apiJSON("/api/file?name=" + encodeURIComponent(currentFile), {
    method: "PUT",
    headers: { "content-type": "text/plain" },
    body: $("#file-text").value,
  });
  $("#edit-name").textContent = currentFile + " ✓已保存";
  loadFiles();
});
$("#files-refresh").addEventListener("click", loadFiles);
$("#upload-input").addEventListener("change", async (e) => {
  const file = e.target.files[0];
  if (!file) return;
  const fd = new FormData();
  fd.append("file", file);
  fd.append("name", file.name);
  await api("/api/upload", { method: "POST", body: fd });
  e.target.value = "";
  loadFiles();
});

// ── results ─────────────────────────────────────────────────────────
function pill(val, type) {
  if (!val) return `<span class="pill neutral">-</span>`;
  const cls = type || (val === "ok" ? "ok" : (val === "k12" ? "k12" : "neutral"));
  return `<span class="pill ${cls}">${val}</span>`;
}
async function loadAccounts() {
  const { accounts, total } = await apiJSON("/api/accounts");
  const k12 = accounts.filter((a) => (a.plan_type || "").toLowerCase() === "k12").length;
  $("#results-summary").textContent = `共 ${total} 个账号 · k12 ${k12}`;
  const tb = $("#accounts-table tbody");
  tb.innerHTML = "";
  accounts.forEach((a) => {
    const tr = document.createElement("tr");
    const planCls = (a.plan_type || "").toLowerCase() === "k12" ? "k12" : "neutral";
    tr.innerHTML = `
      <td>${a.email || "-"}</td>
      <td>${pill(a.plan_type, planCls)}</td>
      <td>${pill(a.join_status)}</td>
      <td>${pill(a.approve_status)}</td>
      <td>${pill(a.elevate_status)}</td>
      <td>${pill(a.import_status)}</td>
      <td class="muted">${(a.chatgpt_account_id || "").slice(0, 12) || "-"}</td>
      <td>${a.has_access_token ? pill("yes", "ok") : pill("no", "bad")}</td>`;
    tb.appendChild(tr);
  });
}
$("#results-refresh").addEventListener("click", loadAccounts);
$("#dl-tokens").addEventListener("click", () => window.open("/api/download?name=access_token.txt", "_blank"));
$("#dl-accounts").addEventListener("click", () => window.open("/api/download?name=registered_accounts.json", "_blank"));

// ── boot ────────────────────────────────────────────────────────────
async function bootApp() {
  const me = await apiJSON("/api/me");
  $("#data-dir").textContent = me.data_dir || "";
  startLogStream();
  refreshRunStatus();
  setInterval(refreshRunStatus, 3000);
}

(async function init() {
  try {
    const me = await apiJSON("/api/me");
    if (me.authed) showApp();
    else showLogin();
  } catch (e) { showLogin(); }
})();
