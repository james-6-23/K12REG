package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k12reg/internal/config"
	"k12reg/internal/mail"
	"k12reg/internal/register"
	"k12reg/internal/storage"
	"k12reg/internal/workspace"
)

// POST /api/workspace/join-owner
// Body:
//
//	{
//	  "manager_session": ...,                 // raw JSON, or omit if manager_session_file set
//	  "manager_session_file": "session.json", // preferred: load from data dir; account.id = workspace
//	  "target_mode": "session" | "register",   // default session
//	  "target_session" | "session": ...,       // when target_mode=session
//	  "mailboxes_file": "hotmail.txt",         // optional; register mode
//	  "set_owner": true,
//	  "proxy": ""
//	}
//
// Workspace ID is always taken from manager session JSON (account.id / account_id),
// never from a separate form field.
//
// Register mode: load email pool with alias_count=1 (no plus-aliases), take the
// last free mailbox, run protocol register, mark used, then join+set-owner.
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
		TargetMode         string `json:"target_mode"`
		MailboxesFile      string `json:"mailboxes_file"`
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

	mode := strings.ToLower(strings.TrimSpace(body.TargetMode))
	if mode == "" {
		mode = "session"
	}

	preLogs := []workspace.LogLine{}

	var target any
	switch mode {
	case "session", "paste", "json":
		target = body.TargetSession
		if target == nil {
			target = body.Session
		}
		if target == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "缺少 target_session"})
			return
		}
	case "register", "pool", "mail":
		// Long-running: registration + OTP. Client has no short timeout by default.
		regTarget, regLogs, regErr := s.registerTargetFromPool(r.Context(), cfg, body.MailboxesFile, proxy)
		preLogs = append(preLogs, regLogs...)
		if regErr != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"ok":    false,
				"error": regErr.Error(),
				"logs":  preLogs,
			})
			return
		}
		target = regTarget
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"detail": "target_mode 应为 session 或 register",
		})
		return
	}

	res := workspace.JoinAndSetOwner(workspace.JoinOwnerRequest{
		ManagerSession: body.ManagerSession,
		TargetSession:  target,
		Proxy:          proxy,
		SetOwner:       setOwner,
		MaxApprove:     maxAttempts,
	})
	// Prepend registration logs so the UI shows the full flow.
	if len(preLogs) > 0 {
		res.Logs = append(preLogs, res.Logs...)
	}

	status := http.StatusOK
	if !res.OK {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, res)
}

// registerTargetFromPool: no aliases, take from pool end, mark used, return session-like map.
func (s *Server) registerTargetFromPool(
	ctx context.Context,
	cfg config.Config,
	mailboxesFile, preferredProxy string,
) (session map[string]any, logs []workspace.LogLine, err error) {
	log := func(level, msg string) {
		logs = append(logs, workspace.LogLine{
			T: time.Now().UnixMilli(), Level: level, Msg: msg,
		})
	}
	if ctx == nil {
		ctx = context.Background()
	}

	name := strings.TrimSpace(mailboxesFile)
	if name == "" {
		name = strings.TrimSpace(cfg.MailboxesFile)
	}
	if name == "" {
		name = firstMailPoolFile(s.opt.DataDir)
	}
	if name == "" {
		return nil, logs, fmt.Errorf("未配置邮箱池文件（请选择 hotmail.txt 等）")
	}

	mailFile := name
	if !filepath.IsAbs(mailFile) {
		mailFile = filepath.Join(s.opt.DataDir, name)
	}
	if st, e := os.Stat(mailFile); e != nil || st.IsDir() {
		return nil, logs, fmt.Errorf("邮箱池文件不存在: %s", name)
	}

	statePath := filepath.Join(s.opt.DataDir, "outlook_token_state.json")
	// Force alias_count=1: use base addresses only, never plus-aliases.
	const aliasCount = 1
	pool, e := mail.LoadPool(mailFile, statePath, aliasCount)
	if e != nil {
		return nil, logs, fmt.Errorf("加载邮箱池: %w", e)
	}

	log("info", fmt.Sprintf("注册模式 · 邮箱池=%s · 无别名 · 从末尾取号 · 可用=%d", name, pool.Available()))

	mb, e := pool.AcquireFromEnd()
	if e != nil {
		return nil, logs, fmt.Errorf("取邮箱: %w", e)
	}
	log("info", fmt.Sprintf("已取邮箱（末尾）· %s · 标记 in_use", mb.Address))

	proxies := loadProxiesForJoinOwner(cfg, preferredProxy)
	proxyRetries := cfg.RegisterProxyRetries
	if proxyRetries < 1 {
		proxyRetries = 1
	}
	if len(proxies) == 0 {
		proxies = []string{""}
	}

	sentinel := resolveSentinelVM(cfg.SentinelVMDir)
	otpTimeout := time.Duration(cfg.WaitTimeout * float64(time.Second))
	if otpTimeout < 5*time.Second {
		otpTimeout = 30 * time.Second
	}
	otpInterval := time.Duration(cfg.WaitInterval * float64(time.Second))
	if otpInterval < 200*time.Millisecond {
		otpInterval = 1500 * time.Millisecond
	}

	var (
		reg     *register.Result
		usedPx  string
		lastErr error
	)
	for attempt := 1; attempt <= proxyRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			pool.Release(mb)
			return nil, logs, err
		}
		px := proxies[(attempt-1)%len(proxies)]
		usedPx = px
		if attempt > 1 {
			log("warn", fmt.Sprintf("注册重试 %d/%d · proxy=%s", attempt, proxyRetries, maskJoinProxy(px)))
		} else {
			log("info", fmt.Sprintf("开始注册 · %s · proxy=%s", mb.Address, maskJoinProxy(px)))
		}
		reg, lastErr = register.Run(register.Options{
			Proxy:         px,
			Mailbox:       mb,
			OTPTimeout:    otpTimeout,
			OTPInterval:   otpInterval,
			SentinelVMDir: sentinel,
			Log: func(s string) {
				log("info", s)
			},
			Ctx: ctx,
		})
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			pool.Release(mb)
			return nil, logs, ctx.Err()
		}
		// Soft network failures: rotate proxy; permanent Graph death: mark used and stop.
		if mail.IsGraphAuthPermanent(lastErr) {
			pool.MarkGraphDead(mb)
			log("err", fmt.Sprintf("注册失败（Graph 死号，已标记 used）: %v", lastErr))
			return nil, logs, fmt.Errorf("注册失败: %w", lastErr)
		}
		if attempt == proxyRetries {
			break
		}
		log("warn", fmt.Sprintf("注册 soft-fail: %v", lastErr))
	}

	if lastErr != nil || reg == nil {
		// Consumed attempt: mark used so the same base is not handed out again
		// (user requirement: 标记为已经使用).
		pool.Mark(mb, true)
		if lastErr == nil {
			lastErr = fmt.Errorf("注册无结果")
		}
		log("err", fmt.Sprintf("注册失败（已标记 used）: %v", lastErr))
		return nil, logs, fmt.Errorf("注册失败: %w", lastErr)
	}

	// Success → mark used (base address, no aliases).
	pool.Mark(mb, true)
	log("ok", fmt.Sprintf("注册成功 · %s · 已标记 used · AT=%v RT=%v",
		reg.Email, reg.AccessToken != "", reg.RefreshToken != ""))

	// Persist like the batch pipeline (best-effort).
	acc := storage.Account{
		"email":         reg.Email,
		"password":      reg.Password,
		"access_token":  reg.AccessToken,
		"refresh_token": reg.RefreshToken,
		"id_token":      reg.IDToken,
		"source_type":   reg.SourceType,
		"created_at":    reg.CreatedAt,
		"via":           "join-owner-register",
	}
	accountsPath := storage.AccountsFile(cfg.DataDir)
	if e := storage.AppendAccount(accountsPath, acc); e != nil {
		log("warn", "保存 registered_accounts: "+e.Error())
	}

	// Session-like blob for ParseSession / JoinAndSetOwner / session_after.
	session = map[string]any{
		"accessToken":   reg.AccessToken,
		"access_token":  reg.AccessToken,
		"refresh_token": reg.RefreshToken,
		"id_token":      reg.IDToken,
		"email":         reg.Email,
		"password":      reg.Password,
		"user": map[string]any{
			"email": reg.Email,
		},
		"source_type": reg.SourceType,
		"created_at":  reg.CreatedAt,
		"via":         "join-owner-register",
	}
	if usedPx != "" {
		session["proxy_used"] = usedPx
	}
	return session, logs, nil
}

func loadProxiesForJoinOwner(cfg config.Config, preferred string) []string {
	var out []string
	if p := strings.TrimSpace(preferred); p != "" {
		out = append(out, p)
	}
	if cfg.Proxy != "" && (len(out) == 0 || !strings.EqualFold(out[0], cfg.Proxy)) {
		out = append(out, cfg.Proxy)
	}
	if cfg.ProxiesFile != "" {
		file := cfg.ResolvePath(cfg.ProxiesFile)
		if file == "" {
			file = filepath.Join(cfg.DataDir, cfg.ProxiesFile)
		}
		if list, e := config.LoadProxies(file, cfg.DefaultProtocol); e == nil {
			for _, p := range list {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				dup := false
				for _, x := range out {
					if x == p {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func resolveSentinelVM(configured string) string {
	if configured != "" {
		return configured
	}
	if v := os.Getenv("K12_SENTINEL_VM"); v != "" {
		return v
	}
	for _, cand := range []string{
		filepath.Join("scripts", "sentinel_vm"),
		"/app/scripts/sentinel_vm",
	} {
		if st, e := os.Stat(filepath.Join(cand, "run_sentinel_vm.js")); e == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

func maskJoinProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "(direct)"
	}
	if len(p) <= 24 {
		return p
	}
	return p[:12] + "…" + p[len(p)-8:]
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
