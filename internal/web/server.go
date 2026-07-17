package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k12reg/internal/storage"
)

var textSuffixes = map[string]bool{
	".toml": true, ".txt": true, ".json": true, ".jsonl": true, ".env": true, ".log": true, ".md": true,
}

// Max file size openable in the online editor (512 KiB).
const maxEditorBytes = 512 * 1024

var hiddenNames = map[string]bool{
	".DS_Store": true, "settings.json": true, ".effective_config.toml": true,
	"schedule.json": true, "task_history.jsonl": true,
}

// Options for the control panel server.
type Options struct {
	DataDir    string
	Password   string
	StaticDir  string // directory with index.html + assets/
	SessionDays int
}

// Server is the Go control panel (replaces FastAPI webapp).
type Server struct {
	opt       Options
	auth      *auth
	runner    *RunManager
	scheduler *Scheduler
	mux       *http.ServeMux
}

func New(opt Options) *Server {
	if opt.SessionDays < 1 {
		opt.SessionDays = 30
	}
	_ = os.MkdirAll(opt.DataDir, 0o755)
	seedDataDir(opt.DataDir)
	runner := NewRunManager(opt.DataDir)
	s := &Server{
		opt:       opt,
		auth:      newAuth(opt.Password, opt.SessionDays),
		runner:    runner,
		scheduler: NewScheduler(opt.DataDir, runner),
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s
}

// seedDataDir creates empty template files if missing so first deploy is usable.
// No host-side mkdir is required: Docker volume mount + server MkdirAll handle the directory.
func seedDataDir(dir string) {
	_ = os.MkdirAll(dir, 0o755)
	type seed struct {
		name string
		body string
	}
	for _, f := range []seed{
		{name: "hotmail.txt", body: "# email----password----refresh_token----client_id\n"},
		{name: "proxies.txt", body: "# host:port:user:pass  or  socks5://user:pass@host:port\n"},
		{name: "hotsession.json", body: "{}\n"},
		{name: "session.json", body: "{}\n"},
	} {
		p := filepath.Join(dir, f.name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		_ = os.WriteFile(p, []byte(f.body), 0o644)
	}
	// Minimal settings only if completely missing (never overwrite user config).
	sp := filepath.Join(dir, settingsFile)
	if _, err := os.Stat(sp); err != nil {
		_ = os.WriteFile(sp, []byte(`{
  "registration": { "mode": "protocol", "total": 1, "threads": 1, "pipeline_gate": "full", "oauth_path": "chatgpt_web" },
  "workspace": { "enabled": true, "ids": [], "selected_id": "", "manager_session_file": "hotsession.json", "approve_requests": true },
  "mail": {
    "mailboxes_file": "hotmail.txt",
    "alias_count": 1,
    "providers": [{ "type": "outlook_token", "enable": true, "mode": "graph", "mailboxes_file": "hotmail.txt", "alias_count": 1 }]
  },
  "proxy": { "proxies_file": "proxies.txt", "default_protocol": "socks5" },
  "import_api": { "require_k12": true, "endpoints": [] }
}
`), 0o644)
	}
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/login", s.handleLogin)
	s.mux.HandleFunc("/api/logout", s.handleLogout)
	s.mux.HandleFunc("/api/me", s.handleMe)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/settings/raw", s.handleSettingsRaw)
	s.mux.HandleFunc("/api/files", s.handleFiles)
	s.mux.HandleFunc("/api/file", s.handleFile)
	s.mux.HandleFunc("/api/upload", s.handleUpload)
	s.mux.HandleFunc("/api/download", s.handleDownload)
	s.mux.HandleFunc("/api/run/status", s.handleRunStatus)
	s.mux.HandleFunc("/api/run/start", s.handleRunStart)
	s.mux.HandleFunc("/api/run/stop", s.handleRunStop)
	s.mux.HandleFunc("/api/run/logs/snapshot", s.handleLogsSnapshot)
	s.mux.HandleFunc("/api/run/logs/clear", s.handleLogsClear)
	s.mux.HandleFunc("/api/run/logs", s.handleLogsSSE)
	s.mux.HandleFunc("/api/accounts", s.handleAccounts)
	s.mux.HandleFunc("/api/schedule", s.handleSchedule)
	s.mux.HandleFunc("/api/mail/pool", s.handleMailPool)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/workspace/join-owner", s.handleJoinOwner)
	s.mux.HandleFunc("/api/workspace/parse-session", s.handleParseSession)
	s.mux.HandleFunc("/", s.handleStatic)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}
	pw := r.FormValue("password")
	if !s.auth.checkPassword(pw) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"detail": "密码错误"})
		return
	}
	tok, err := s.auth.issue()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.auth.setCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	s.auth.clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authed":   s.auth.valid(s.auth.fromRequest(r)),
		"data_dir": s.opt.DataDir,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, getEffectiveSettings(s.opt.DataDir))
	case http.MethodPut:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "请求体必须是 JSON"})
			return
		}
		saved, err := saveOverlayForm(s.opt.DataDir, body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": saved})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) handleSettingsRaw(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(loadOverlayText(s.opt.DataDir)))
	case http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		if err := saveOverlayText(s.opt.DataDir, string(b)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": s.listFiles()})
}

func (s *Server) listFiles() []map[string]any {
	// Always return a non-nil slice so JSON is [] not null (empty data dir / read error).
	out := make([]map[string]any, 0)
	_ = os.MkdirAll(s.opt.DataDir, 0o755)
	entries, err := os.ReadDir(s.opt.DataDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || hiddenNames[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		isText := textSuffixes[ext]
		// Huge dumps are download-only in the UI editor.
		editable := isText && info.Size() <= maxEditorBytes
		// Prefer not editing account dumps at all (always large / machine-generated).
		if e.Name() == storage.AccountsJSON || e.Name() == storage.AccountsJSONL ||
			e.Name() == storage.AccountsJSON+".bak" || strings.HasPrefix(e.Name(), "registered_accounts") {
			editable = false
		}
		out = append(out, map[string]any{
			"name":     e.Name(),
			"size":     info.Size(),
			"mtime":    float64(info.ModTime().Unix()),
			"editable": editable,
			"text":     isText,
		})
	}
	return out
}

func (s *Server) safePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("非法文件名")
	}
	p := filepath.Join(s.opt.DataDir, name)
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	root, _ := filepath.Abs(s.opt.DataDir)
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		return "", fmt.Errorf("文件必须位于数据目录内")
	}
	return abs, nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	p, err := s.safePath(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := os.Stat(p)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"detail": "文件不存在"})
			return
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !textSuffixes[ext] {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"detail": "非文本文件，请使用下载"})
			return
		}
		// Block opening huge dumps in the editor (use download instead).
		base := filepath.Base(p)
		if st.Size() > maxEditorBytes ||
			base == storage.AccountsJSON || base == storage.AccountsJSONL ||
			strings.HasPrefix(base, "registered_accounts") {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"detail": fmt.Sprintf("文件过大（%s），请使用下载查看。账号列表请到「结果」页浏览。", fmtSize(st.Size())),
				"size":   st.Size(),
				"max":    maxEditorBytes,
			})
			return
		}
		b, err := os.ReadFile(p)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
	case http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		_ = os.Remove(p)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "missing file"})
		return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" {
		name = hdr.Filename
	}
	p, err := s.safePath(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	b, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": filepath.Base(p), "size": len(b)})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	// Export legacy name from JSONL when requested.
	if name == storage.AccountsJSON {
		tmp := filepath.Join(s.opt.DataDir, ".export_accounts.json")
		if err := storage.ExportAccountsJSON(s.opt.DataDir, tmp); err == nil {
			w.Header().Set("Content-Disposition", "attachment; filename="+storage.AccountsJSON)
			http.ServeFile(w, r, tmp)
			return
		}
		// fall through to file if export failed (legacy json may still exist)
	}
	p, err := s.safePath(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "文件不存在"})
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(p))
	http.ServeFile(w, r, p)
}

func fmtSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var body struct {
		Count       *int   `json:"count"`
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var count *int
	if body.Count != nil && *body.Count > 0 {
		count = body.Count
	}
	// Persist selected workspace when provided so next run remembers the choice.
	wsID := strings.TrimSpace(body.WorkspaceID)
	if wsID != "" {
		_ = persistWorkspaceSelected(s.opt.DataDir, wsID)
	}
	if err := s.runner.Start(count, wsID, "manual"); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
				if limit > 500 {
					limit = 500
				}
			}
		}
		recs, err := loadTaskRecords(s.opt.DataDir, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
		if recs == nil {
			recs = []TaskRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tasks":   recs,
			"summary": taskSummary(recs),
			"file":    taskHistoryFile,
		})
	case http.MethodDelete:
		if err := clearTaskRecords(s.opt.DataDir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) handleRunStop(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	stopped := s.runner.Stop(body.Force)
	st := s.runner.Status()
	st["stopped"] = stopped
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleLogsSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": s.runner.Snapshot()})
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	n := s.runner.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
}

func (s *Server) handleLogsSSE(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// replay buffer
	for _, line := range s.runner.Snapshot() {
		writeSSE(w, line, "")
	}
	flusher.Flush()

	ch := s.runner.Subscribe()
	defer s.runner.Unsubscribe(ch)

	// heartbeat
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case line, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, line, "")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, data, event string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.scheduler.Get())
	case http.MethodPut:
		var body ScheduleConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid JSON"})
			return
		}
		cfg, err := s.scheduler.Update(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	// Pagination: ?limit=100&offset=0&order=desc|asc (default desc = newest first)
	limit := 100
	offset := 0
	orderDesc := true
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	if r.URL.Query().Get("order") == "asc" {
		orderDesc = false
	}
	rows, total, err := storage.LoadAccountPage(s.opt.DataDir, offset, limit, orderDesc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if rows == nil {
		rows = []storage.AccountSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"order":    map[bool]string{true: "desc", false: "asc"}[orderDesc],
	})
}

func strNonEmpty(v any) bool {
	s, _ := v.(string)
	return strings.TrimSpace(s) != ""
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	root := s.opt.StaticDir
	if root == "" {
		http.Error(w, "Frontend not built", http.StatusServiceUnavailable)
		return
	}
	// serve assets and index
	upath := r.URL.Path
	if upath == "/" || upath == "" {
		idx := filepath.Join(root, "index.html")
		if _, err := os.Stat(idx); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>K12REG</title>
<body style="font-family:system-ui;background:#0a0d12;color:#e2e8f0;display:flex;min-height:100vh;align-items:center;justify-content:center">
<div style="max-width:36rem;padding:2rem"><h1>Frontend not built</h1>
<p style="color:#94a3b8">Run <code>cd frontend && npm run build</code> then restart (serves frontend/dist)</p></div></body>`))
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, idx)
		return
	}
	// prevent path escape
	clean := filepath.Clean("/" + upath)
	full := filepath.Join(root, clean)
	if !strings.HasPrefix(full, filepath.Clean(root)) {
		http.NotFound(w, r)
		return
	}
	if st, err := os.Stat(full); err == nil && !st.IsDir() {
		http.ServeFile(w, r, full)
		return
	}
	// SPA fallback
	http.ServeFile(w, r, filepath.Join(root, "index.html"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
