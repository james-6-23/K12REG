package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k12reg/internal/config"
	"k12reg/internal/mail"
	"k12reg/internal/pipeline"
	"k12reg/internal/workspace"
)

// RunManager runs the Go pipeline in-process and fans out logs for SSE.
type RunManager struct {
	dataDir string

	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
	startedAt time.Time
	exitCode  *int
	buf       []string
	maxBuf    int
	subs      map[chan string]struct{}
	// current run metadata (for task history)
	runSource   string // manual | schedule
	runCount    *int
	runWSID     string
}

func NewRunManager(dataDir string) *RunManager {
	return &RunManager{
		dataDir: dataDir,
		maxBuf:  5000,
		subs:    map[chan string]struct{}{},
	}
}

func (r *RunManager) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var elapsed any
	if r.running && !r.startedAt.IsZero() {
		elapsed = time.Since(r.startedAt).Seconds()
	}
	var exit any
	if r.exitCode != nil {
		exit = *r.exitCode
	}
	var started any
	if !r.startedAt.IsZero() {
		started = float64(r.startedAt.Unix())
	}
	return map[string]any{
		"running":        r.running,
		"pid":            nil,
		"started_at":     started,
		"elapsed":        elapsed,
		"command":        "k12reg pipeline (in-process)",
		"exit_code":      exit,
		"buffered_lines": len(r.buf),
	}
}

func (r *RunManager) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.buf))
	copy(out, r.buf)
	return out
}

// ClearLogs drops the in-memory log buffer so SSE replay stays empty after 清屏.
func (r *RunManager) ClearLogs() int {
	r.mu.Lock()
	n := len(r.buf)
	r.buf = nil
	r.mu.Unlock()
	return n
}

func (r *RunManager) Subscribe() chan string {
	ch := make(chan string, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch
}

func (r *RunManager) Unsubscribe(ch chan string) {
	r.mu.Lock()
	delete(r.subs, ch)
	r.mu.Unlock()
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (r *RunManager) emit(line string) {
	// Stamp server local time at emit so UI timestamps follow real event order.
	// Client-side Date() would collapse to one second when SSE replays the buffer.
	stamped := time.Now().Format("15:04:05.000") + "\t" + line
	r.mu.Lock()
	r.buf = append(r.buf, stamped)
	if len(r.buf) > r.maxBuf {
		r.buf = r.buf[len(r.buf)-r.maxBuf:]
	}
	subs := make([]chan string, 0, len(r.subs))
	for ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- stamped:
		default:
		}
	}
}

// Start launches the pipeline.
// count overrides settings total when non-nil.
// workspaceID overrides workspace.selected_id when non-empty (and is used for join).
// source is "manual" or "schedule" (task history).
func (r *RunManager) Start(count *int, workspaceID string, source string) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("pipeline already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.running = true
	r.cancel = cancel
	r.startedAt = time.Now()
	r.exitCode = nil
	r.buf = nil
	src := strings.TrimSpace(source)
	if src == "" {
		src = "manual"
	}
	r.runSource = src
	r.runCount = count
	r.runWSID = strings.TrimSpace(workspaceID)
	r.mu.Unlock()

	label := "手动"
	if src == "schedule" {
		label = "定时"
	}
	r.emit(fmt.Sprintf("▶ 启动流水线 (%s · Go in-process)", label))
	go r.run(ctx, count, strings.TrimSpace(workspaceID), src)
	return nil
}

func (r *RunManager) run(ctx context.Context, count *int, workspaceID, source string) {
	code := 0
	started := time.Now()
	rec := TaskRecord{
		ID:        newTaskID(started),
		Source:    source,
		StartedAt: started.UTC().Format(time.RFC3339),
		Status:    "error",
	}
	if source == "" {
		rec.Source = "manual"
	}

	defer func() {
		finished := time.Now()
		rec.FinishedAt = finished.UTC().Format(time.RFC3339)
		rec.ElapsedSec = finished.Sub(started).Seconds()
		rec.ExitCode = code
		if rec.Status == "" {
			rec.Status = "error"
		}
		if err := appendTaskRecord(r.dataDir, rec); err != nil {
			r.emit("· task history save: " + err.Error())
		}
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.exitCode = &code
		r.mu.Unlock()
		r.emit(fmt.Sprintf("■ 流水线结束 (exit=%d · reg=%d fail=%d · %s)",
			code, rec.Registered, rec.Fail, rec.Status))
	}()

	settingsPath := filepath.Join(r.dataDir, "settings.json")
	cfg, err := config.LoadJSON(settingsPath)
	if err != nil {
		cfg = config.Default()
	}
	cfg.DataDir = r.dataDir
	if count != nil && *count > 0 {
		cfg.Total = *count
	}
	rec.Threads = cfg.Threads

	if cfg.SentinelVMDir == "" {
		cfg.SentinelVMDir = os.Getenv("K12_SENTINEL_VM")
	}
	if cfg.SentinelVMDir == "" {
		for _, cand := range []string{
			filepath.Join("scripts", "sentinel_vm"),
			"/app/scripts/sentinel_vm",
		} {
			if st, e := os.Stat(filepath.Join(cand, "run_sentinel_vm.js")); e == nil && !st.IsDir() {
				cfg.SentinelVMDir = cand
				break
			}
		}
	}

	// Global mail fallback name
	if cfg.MailboxesFile == "" {
		if name := firstMailPoolFile(r.dataDir); name != "" {
			cfg.MailboxesFile = name
		}
	}

	var proxies []string
	if cfg.Proxy != "" {
		proxies = []string{cfg.Proxy}
	} else if cfg.ProxiesFile != "" {
		if px, e := config.LoadProxies(cfg.ResolvePath(cfg.ProxiesFile), cfg.DefaultProtocol); e == nil {
			proxies = px
		}
	}

	// Build manager slots (multi-workspace) or legacy single.
	slots := r.resolveManagerSlots(cfg, workspaceID, count)
	if cfg.WorkspaceEnabled && len(slots) == 0 {
		r.emit("✗ 未配置母号 session（设置 → Workspace → 添加母号）")
		code = 1
		rec.Status = "error"
		rec.Note = "未配置母号 session"
		return
	}

	// Shared mail pool: all managers must share one domain when RequireSameDomain.
	if cfg.WorkspaceEnabled && !cfg.IsPerManagerMail() && cfg.RequireSameDomain && len(slots) > 1 {
		doms := map[string]struct{}{}
		for _, s := range slots {
			if d := strings.TrimSpace(s.Domain); d != "" {
				doms[d] = struct{}{}
			}
		}
		if len(doms) > 1 {
			r.emit("✗ 共用邮箱池模式要求各母号邮箱域名一致；当前域名不一致，请改用「每母号绑定邮箱池」或统一母号域名")
			for _, s := range slots {
				r.emit(fmt.Sprintf("  · %s · @%s · %s", s.SessionFile, s.Domain, truncID(s.WorkspaceID, 12)))
			}
			code = 1
			rec.Status = "error"
			rec.Note = "母号域名不一致"
			return
		}
	}

	requested := 0
	for _, s := range slots {
		requested += s.Quota
	}
	if !cfg.WorkspaceEnabled {
		// register-only: single pass with cfg.Total
		if requested < 1 {
			requested = cfg.Total
		}
	}
	rec.Requested = requested
	if len(slots) > 0 {
		rec.WorkspaceID = slots[0].WorkspaceID
		rec.MailboxesFile = firstNonEmpty(slots[0].MailboxesFile, cfg.MailboxesFile)
	} else {
		rec.MailboxesFile = cfg.MailboxesFile
	}

	binding := "shared"
	if cfg.IsPerManagerMail() {
		binding = "per_manager"
	}
	r.emit(fmt.Sprintf("· managers=%d · mail_binding=%s · total_quota=%d · threads=%d",
		len(slots), binding, requested, cfg.Threads))

	statePath := filepath.Join(r.dataDir, "outlook_token_state.json")
	var (
		sumReg, sumFail, sumJoin, sumApprove, sumK12, sumImport int64
	)

	runOne := func(slot config.ManagerSlot, tag string) (pipeline.Stats, error) {
		slotCfg := cfg
		slotCfg.Total = slot.Quota
		if slotCfg.Total < 1 {
			slotCfg.Total = 1
		}
		slotCfg.ManagerSessionFile = slot.SessionFile
		slotCfg.WorkspaceSelectedID = slot.WorkspaceID
		slotCfg.WorkspaceIDs = nil
		if slot.WorkspaceID != "" {
			slotCfg.WorkspaceIDs = []string{slot.WorkspaceID}
		}
		mailName := strings.TrimSpace(slot.MailboxesFile)
		if mailName == "" || !cfg.IsPerManagerMail() {
			mailName = cfg.MailboxesFile
		}
		if mailName == "" {
			return pipeline.Stats{}, fmt.Errorf("未配置邮箱池")
		}
		slotCfg.MailboxesFile = mailName
		mailFile := slotCfg.ResolvePath(mailName)
		if mailFile == "" {
			mailFile = filepath.Join(r.dataDir, mailName)
		}
		pool, e := mail.LoadPool(mailFile, statePath, slotCfg.AliasCount)
		if e != nil {
			return pipeline.Stats{}, e
		}
		r.emit(fmt.Sprintf("%s mail=%s · available=%d · ws=%s · @%s · quota=%d",
			tag, mailName, pool.Available(), truncID(slot.WorkspaceID, 12),
			orDash(slot.Domain), slot.Quota))
		return pipeline.Run(pipeline.Options{
			Config:  slotCfg,
			Proxies: proxies,
			Pool:    pool,
			Ctx:     ctx,
			Log:     func(s string) { r.emit(s) },
		})
	}

	if !cfg.WorkspaceEnabled || len(slots) == 0 {
		// No workspace: classic single mail pool run
		if cfg.MailboxesFile == "" {
			r.emit("✗ mail pool: 未配置邮箱池文件")
			code = 1
			rec.Status = "error"
			rec.Note = "未配置邮箱池文件"
			return
		}
		st, err := runOne(config.ManagerSlot{
			Enabled: true, Quota: cfg.Total, MailboxesFile: cfg.MailboxesFile,
		}, "·")
		sumReg, sumFail = st.Registered, st.Fail
		sumJoin, sumApprove, sumK12, sumImport = st.JoinOK, st.ApproveOK, st.K12, st.ImportOK
		if err != nil {
			if ctx.Err() != nil {
				r.emit("⏹ 已取消")
				code = 130
				rec.Status = "cancelled"
				rec.Note = "用户停止 / 取消"
				return
			}
			r.emit("✗ pipeline: " + err.Error())
			code = 1
			rec.Status = "error"
			rec.Note = err.Error()
			return
		}
	} else {
		for i, slot := range slots {
			if ctx.Err() != nil {
				r.emit("⏹ 已取消")
				code = 130
				rec.Status = "cancelled"
				rec.Note = "用户停止 / 取消"
				return
			}
			tag := fmt.Sprintf("[%d/%d]", i+1, len(slots))
			r.emit(fmt.Sprintf("── 母号 %s · %s · email=%s · quota=%d",
				tag, slot.SessionFile, orDash(slot.Email), slot.Quota))
			st, err := runOne(slot, tag)
			sumReg += st.Registered
			sumFail += st.Fail
			sumJoin += st.JoinOK
			sumApprove += st.ApproveOK
			sumK12 += st.K12
			sumImport += st.ImportOK
			if err != nil {
				if ctx.Err() != nil {
					r.emit("⏹ 已取消")
					code = 130
					rec.Status = "cancelled"
					rec.Note = "用户停止 / 取消"
					return
				}
				r.emit(fmt.Sprintf("%s pipeline: %v", tag, err))
				// continue other managers unless fatal cancel
				sumFail++
				continue
			}
			r.emit(fmt.Sprintf("%s done · reg=%d join=%d fail=%d",
				tag, st.Registered, st.JoinOK, st.Fail))
		}
	}

	rec.Registered = sumReg
	rec.Fail = sumFail
	rec.JoinOK = sumJoin
	rec.ApproveOK = sumApprove
	rec.K12 = sumK12
	rec.ImportOK = sumImport
	r.emit(fmt.Sprintf("── summary registered=%d join=%d k12=%d fail=%d",
		sumReg, sumJoin, sumK12, sumFail))
	if sumReg == 0 && sumFail > 0 {
		code = 2
		rec.Status = "fail"
		rec.Note = "全部失败"
		return
	}
	if sumFail > 0 && sumReg > 0 {
		rec.Status = "ok"
		rec.Note = "部分成功"
		return
	}
	rec.Status = "ok"
	if sumReg == 0 && sumFail == 0 {
		rec.Note = "无任务完成"
	}
}

// resolveManagerSlots loads session files, fills workspace id / domain, applies quota overrides.
// count: when set, every manager uses this quota for the run.
// workspaceID: when set, only that workspace (or single legacy override).
func (r *RunManager) resolveManagerSlots(cfg config.Config, workspaceID string, count *int) []config.ManagerSlot {
	raw := cfg.ActiveManagers()
	// If request forces a single workspace id and managers empty of match, filter later.
	var out []config.ManagerSlot
	for _, slot := range raw {
		sf := strings.TrimSpace(slot.SessionFile)
		if sf != "" {
			path := cfg.ResolvePath(sf)
			if path == "" {
				path = filepath.Join(r.dataDir, sf)
			}
			if m, e := workspace.LoadManagerSession(path); e == nil {
				if m.AccountID != "" {
					slot.WorkspaceID = m.AccountID
				}
				if m.Email != "" {
					slot.Email = m.Email
					slot.Domain = config.EmailDomain(m.Email)
				}
			} else {
				r.emit(fmt.Sprintf("· session load %s: %v", sf, e))
			}
		}
		if workspaceID != "" && slot.WorkspaceID != "" &&
			!strings.EqualFold(slot.WorkspaceID, workspaceID) {
			continue
		}
		if workspaceID != "" && slot.WorkspaceID == "" {
			slot.WorkspaceID = workspaceID
		}
		if count != nil && *count > 0 {
			slot.Quota = *count
		}
		if slot.Quota < 1 {
			slot.Quota = 1
		}
		// per_manager: keep slot.MailboxesFile; shared: clear so runOne uses global
		if !cfg.IsPerManagerMail() {
			slot.MailboxesFile = ""
		}
		out = append(out, slot)
	}
	// Explicit single workspace + no managers matched: synthesize one slot
	if len(out) == 0 && workspaceID != "" {
		q := cfg.Total
		if count != nil && *count > 0 {
			q = *count
		}
		out = append(out, config.ManagerSlot{
			Enabled:       true,
			SessionFile:   cfg.ManagerSessionFile,
			Quota:         q,
			WorkspaceID:   workspaceID,
			MailboxesFile: "",
		})
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func orDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	return s
}

func truncID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// firstMailPoolFile picks any .txt in dataDir that is not a known non-mail file.
// Filename is free-form (1.txt / outlook.txt / hotmail.txt); only content format matters.
func firstMailPoolFile(dataDir string) string {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}
	skip := map[string]bool{
		"access_token.txt": true,
		"proxies.txt":      true,
	}
	// Prefer common names first, then any other .txt
	prefer := []string{"hotmail.txt", "outlook.txt", "mail.txt", "mailbox.txt"}
	for _, name := range prefer {
		p := filepath.Join(dataDir, name)
		if st, e := os.Stat(p); e == nil && !st.IsDir() && st.Size() > 0 {
			return name
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".txt") || skip[strings.ToLower(name)] {
			continue
		}
		if st, err := e.Info(); err == nil && st.Size() > 0 {
			return name
		}
	}
	return ""
}

func (r *RunManager) Stop(force bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running || r.cancel == nil {
		return false
	}
	r.cancel()
	msg := "⏹ 收到停止请求（优雅取消）"
	if force {
		msg = "⏹ 收到停止请求（强制取消）"
	}
	// emit after unlock via goroutine-safe path
	line := msg
	go r.emit(line)
	return true
}
