package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k12reg/internal/config"
	"k12reg/internal/workspace"
)

// POST /api/workspace/join-owner
// Body:
//
//	{
//	  "manager_session": ...,                 // raw JSON, or omit if manager_session_file set
//	  "manager_session_file": "session.json", // preferred: load from data dir; account.id = workspace
//	  "target_session" | "session": ...,       // target from chatgpt.com/api/auth/session
//	  "set_owner": true,
//	  "proxy": ""
//	}
//
// Target must be a real browser/auth session JSON (user copies from /api/auth/session).
// Registration from email pool is not supported here.
//
// Streaming: send Accept: text/event-stream or ?stream=1 for live log SSE
// (event: log / event: result / event: error).
func (s *Server) handleJoinOwner(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}

	var body struct {
		ManagerSession     any    `json:"manager_session"`
		ManagerSessionFile string `json:"manager_session_file"`
		TargetSession      any    `json:"target_session"`
		Session            any    `json:"session"` // alias for target
		SetOwner           *bool  `json:"set_owner"`
		Proxy              string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "请求体必须是 JSON"})
		return
	}

	// Prefer file on disk so account.id is always the latest from the selected JSON.
	if name := strings.TrimSpace(body.ManagerSessionFile); name != "" {
		raw, err := s.loadSessionFile(name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "母号文件: " + err.Error()})
			return
		}
		body.ManagerSession = raw
	}

	if body.ManagerSession == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"detail": "缺少母号：请选择 manager_session_file 或粘贴 manager_session",
		})
		return
	}

	target := body.TargetSession
	if target == nil {
		target = body.Session
	}
	if target == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"detail": "缺少 target_session（请粘贴 chatgpt.com/api/auth/session JSON）",
		})
		return
	}

	setOwner := true
	if body.SetOwner != nil {
		setOwner = *body.SetOwner
	}

	proxy := strings.TrimSpace(body.Proxy)
	if proxy == "" {
		proxy = pickProxyFromDataDir(s.opt.DataDir)
	}

	settingsPath := filepath.Join(s.opt.DataDir, settingsFile)
	cfg, err := config.LoadJSON(settingsPath)
	if err != nil {
		cfg = config.Default()
	}
	cfg.DataDir = s.opt.DataDir
	maxAttempts := cfg.ApproveMaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 12
	}

	// Live SSE stream for UI.
	wantStream := r.URL.Query().Get("stream") == "1" ||
		strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	var (
		flusher http.Flusher
		stream  bool
	)
	if wantStream {
		if f, ok := w.(http.Flusher); ok {
			stream = true
			flusher = f
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-transform")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
		}
	}

	writeSSE := func(event string, v any) {
		if !stream {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	collected := []workspace.LogLine{}
	onLog := func(line workspace.LogLine) {
		collected = append(collected, line)
		writeSSE("log", line)
	}

	res := workspace.JoinAndSetOwner(workspace.JoinOwnerRequest{
		ManagerSession: body.ManagerSession,
		TargetSession:  target,
		Proxy:          proxy,
		SetOwner:       setOwner,
		MaxApprove:     maxAttempts,
		OnLog:          onLog,
	})
	if len(collected) > 0 {
		res.Logs = collected
	}

	if stream {
		writeSSE("result", res)
		return
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, res)
}

// loadSessionFile reads a session JSON from the data directory (basename only).
func (s *Server) loadSessionFile(name string) (any, error) {
	p, err := s.safePath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("无法读取 %s: %w", name, err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON", name)
	}
	return raw, nil
}

// POST /api/workspace/parse-session
// Body: { session } — validate and summarize session JSON
func (s *Server) handleParseSession(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var body struct {
		Session any `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "请求体必须是 JSON"})
		return
	}
	ps, err := workspace.ParseSession(body.Session)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"email":            ps.Email,
		"user_id":          ps.UserID,
		"account_id":       ps.AccountID,
		"plan_type":        ps.PlanType,
		"expires":          ps.Expires,
		"has_access_token": ps.AccessToken != "",
	})
}

func pickProxyFromDataDir(dataDir string) string {
	settingsPath := filepath.Join(dataDir, settingsFile)
	cfg, err := config.LoadJSON(settingsPath)
	if err != nil {
		cfg = config.Default()
	}
	file := cfg.ProxiesFile
	if file == "" {
		file = "proxies.txt"
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(dataDir, file)
	}
	list, err := config.LoadProxies(file, cfg.DefaultProtocol)
	if err != nil || len(list) == 0 {
		return strings.TrimSpace(cfg.Proxy)
	}
	return list[0]
}
