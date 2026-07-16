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
// Never re-acquires the same address: seeds used from existing session files / accounts,
// and on soft-fail switches to the next free mailbox (not proxy-only retry on one email).
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

	// Seed used from data dir so already-registered / session-json emails are skipped.
	seeded := pool.SeedUsed(collectKnownEmails(s.opt.DataDir))
	if seeded > 0 {
		log("info", fmt.Sprintf("已根据数据目录 session/账号库标记 used · %d 个（避免重复取号）", seeded))
	}

	log("info", fmt.Sprintf("注册模式 · 邮箱池=%s · 无别名 · 从末尾取号 · 可用=%d", name, pool.Available()))

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

	// Try up to N different mailboxes (not N proxies on the same one).
	maxMailboxes := proxyRetries
	if maxMailboxes < 1 {
		maxMailboxes = 1
	}
	if maxMailboxes > 5 {
		maxMailboxes = 5
	}

	var (
		reg     *register.Result
		usedPx  string
		lastErr error
		lastMB  mail.Mailbox
	)

	for mailTry := 1; mailTry <= maxMailboxes; mailTry++ {
		if err := ctx.Err(); err != nil {
			return nil, logs, err
		}
		mb, acqErr := pool.AcquireFromEnd()
		if acqErr != nil {
			if lastErr != nil {
				return nil, logs, fmt.Errorf("取邮箱失败（上一号: %v）: %w", lastErr, acqErr)
			}
			return nil, logs, fmt.Errorf("取邮箱: %w", acqErr)
		}
		lastMB = mb
		log("info", fmt.Sprintf("已取邮箱（末尾 #%d/%d）· %s · in_use", mailTry, maxMailboxes, mb.Address))

		reg = nil
		lastErr = nil
		// One primary attempt + limited proxy rotate on pure network errors only.
		for attempt := 1; attempt <= proxyRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				pool.Release(mb)
				return nil, logs, err
			}
			px := proxies[(attempt-1)%len(proxies)]
			usedPx = px
			if attempt > 1 {
				log("warn", fmt.Sprintf("同邮箱换代理重试 %d/%d · %s · proxy=%s",
					attempt, proxyRetries, mb.Address, maskJoinProxy(px)))
			} else {
				log("info", fmt.Sprintf("开始注册 · %s · proxy=%s", mb.Address, maskJoinProxy(px)))
			}
			reg, lastErr = register.Run(register.Options{
				Proxy:         px,
				Mailbox:       mb,
				OTPTimeout:    otpTimeout,
				OTPInterval:   otpInterval,
				SentinelVMDir: sentinel,
				Log:           func(s string) { log("info", s) },
				Ctx:           ctx,
			})
			if lastErr == nil {
				break
			}
			if ctx.Err() != nil {
				pool.Release(mb)
				return nil, logs, ctx.Err()
			}
			if mail.IsGraphAuthPermanent(lastErr) {
				pool.MarkGraphDead(mb)
				log("err", fmt.Sprintf("注册失败（Graph 死号，已标记 used）· %s · %v", mb.Address, lastErr))
				lastErr = fmt.Errorf("注册失败: %w", lastErr)
				reg = nil
				break // next mailbox
			}
			// invalid_auth_step / OTP / business: burn this mailbox, try next address
			if !isNetworkyRegisterErr(lastErr) || attempt == proxyRetries {
				log("warn", fmt.Sprintf("邮箱 %s 失败，标记 used 并换号: %v", mb.Address, lastErr))
				break
			}
			log("warn", fmt.Sprintf("网络 soft-fail（仍用 %s）: %v", mb.Address, lastErr))
		}

		if lastErr == nil && reg != nil {
			// Success
			pool.MarkUsed(mb)
			log("ok", fmt.Sprintf("注册成功 · %s · 已标记 used · AT=%v RT=%v",
				reg.Email, reg.AccessToken != "", reg.RefreshToken != ""))

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
			if e := storage.AppendAccount(storage.AccountsFile(cfg.DataDir), acc); e != nil {
				log("warn", "保存 registered_accounts: "+e.Error())
			}
			session = map[string]any{
				"accessToken":   reg.AccessToken,
				"refresh_token": reg.RefreshToken,
				"id_token":      reg.IDToken,
				"password":      reg.Password,
				"user":          map[string]any{"email": reg.Email},
				"created_at":    reg.CreatedAt,
			}
			_ = usedPx
			return session, logs, nil
		}

		// Failed this mailbox → always mark used so it is never re-taken.
		pool.MarkUsed(mb)
		log("warn", fmt.Sprintf("已丢弃邮箱 %s（used）· 准备取下一号", mb.Address))
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("注册无结果")
	}
	log("err", fmt.Sprintf("注册失败（已尝试 %d 个邮箱，末号 %s）: %v",
		maxMailboxes, lastMB.Address, lastErr))
	return nil, logs, fmt.Errorf("注册失败: %w", lastErr)
}

// collectKnownEmails gathers addresses that must not be re-registered:
// email-named session json files + registered_accounts rows.
func collectKnownEmails(dataDir string) []string {
	seen := map[string]struct{}{}
	add := func(e string) {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || !strings.Contains(e, "@") {
			return
		}
		seen[e] = struct{}{}
	}

	// 1) data/*.json whose basename looks like an email
	entries, _ := os.ReadDir(dataDir)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		low := strings.ToLower(name)
		if !strings.HasSuffix(low, ".json") {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if strings.Contains(base, "@") {
			add(base)
		}
		// Also parse session JSON for user.email / email
		if b, err := os.ReadFile(filepath.Join(dataDir, name)); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				if e, _ := m["email"].(string); e != "" {
					add(e)
				}
				if u, ok := m["user"].(map[string]any); ok {
					if e, _ := u["email"].(string); e != "" {
						add(e)
					}
				}
			}
		}
	}

	// 2) registered_accounts summaries (paginated load all in chunks)
	for offset := 0; ; offset += 500 {
		rows, total, err := storage.LoadAccountPage(dataDir, offset, 500, true)
		if err != nil || len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if e, ok := r.Email.(string); ok {
				add(e)
			}
		}
		if offset+len(rows) >= total {
			break
		}
	}

	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	return out
}

// isNetworkyRegisterErr: only these warrant proxy rotate on the same mailbox.
func isNetworkyRegisterErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Business / auth-step failures → switch mailbox, don't re-hammer same email.
	for _, k := range []string{
		"invalid_auth_step", "invalid authorization step",
		"otp timeout", "validate_otp", "wrong code", "wrong_email_otp",
		"registration_disallowed", "user_already", "graph auth",
		"turnstile.dx", "compromised",
	} {
		if strings.Contains(s, k) {
			return false
		}
	}
	for _, k := range []string{
		"handshake", "tls", "connection", "eof", "reset", "proxy", "network",
		"i/o timeout", "deadline exceeded", "503", "502", "429", "cloudflare",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
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
