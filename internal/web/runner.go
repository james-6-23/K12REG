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
	rec.Requested = cfg.Total
	rec.Threads = cfg.Threads
	if workspaceID != "" {
		cfg.WorkspaceSelectedID = workspaceID
		// Keep selected in the pool for display consistency.
		found := false
		for _, id := range cfg.WorkspaceIDs {
			if strings.EqualFold(id, workspaceID) {
				found = true
				break
			}
		}
		if !found {
			cfg.WorkspaceIDs = append([]string{workspaceID}, cfg.WorkspaceIDs...)
		}
	}
	rec.WorkspaceID = cfg.ActiveWorkspaceID()
	if cfg.SentinelVMDir == "" {
		cfg.SentinelVMDir = os.Getenv("K12_SENTINEL_VM")
	}
	if cfg.SentinelVMDir == "" {
		// default next to common deploy layout
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

	mailFile := cfg.ResolvePath(cfg.MailboxesFile)
	if mailFile == "" || cfg.MailboxesFile == "" {
		// Fallback: first non-empty .txt that looks like a mail pool (any filename).
		if name := firstMailPoolFile(r.dataDir); name != "" {
			mailFile = filepath.Join(r.dataDir, name)
			cfg.MailboxesFile = name
		}
	}
	if cfg.MailboxesFile == "" {
		r.emit("✗ mail pool: 未配置邮箱池文件（设置里选择，或上传任意 .txt 邮箱池）")
		code = 1
		rec.Status = "error"
		rec.Note = "未配置邮箱池文件"
		return
	}
	rec.MailboxesFile = cfg.MailboxesFile
	r.emit(fmt.Sprintf("· mailboxes_file=%s", cfg.MailboxesFile))
	pool, err := mail.LoadPool(mailFile, filepath.Join(r.dataDir, "outlook_token_state.json"), cfg.AliasCount)
	if err != nil {
		r.emit("✗ mail pool: " + err.Error())
		code = 1
		rec.Status = "error"
		rec.Note = err.Error()
		return
	}

	var proxies []string
	if cfg.Proxy != "" {
		proxies = []string{cfg.Proxy}
	} else if cfg.ProxiesFile != "" {
		if px, e := config.LoadProxies(cfg.ResolvePath(cfg.ProxiesFile), cfg.DefaultProtocol); e == nil {
			proxies = px
		}
	}

	if cfg.WorkspaceEnabled {
		if wid := cfg.ActiveWorkspaceID(); wid != "" {
			r.emit(fmt.Sprintf("· workspace=%s (pool=%d)", truncID(wid, 12), len(cfg.WorkspaceIDs)))
		} else {
			r.emit("· workspace enabled but no selected_id / ids")
		}
	}

	st, err := pipeline.Run(pipeline.Options{
		Config:  cfg,
		Proxies: proxies,
		Pool:    pool,
		Ctx:     ctx,
		Log:     func(s string) { r.emit(s) },
	})
	rec.Registered = st.Registered
	rec.Fail = st.Fail
	rec.JoinOK = st.JoinOK
	rec.ApproveOK = st.ApproveOK
	rec.K12 = st.K12
	rec.ImportOK = st.ImportOK
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
	r.emit(fmt.Sprintf("── summary registered=%d join=%d k12=%d fail=%d",
		st.Registered, st.JoinOK, st.K12, st.Fail))
	if st.Registered == 0 && st.Fail > 0 {
		code = 2
		rec.Status = "fail"
		rec.Note = "全部失败"
		return
	}
	if st.Fail > 0 && st.Registered > 0 {
		rec.Status = "ok"
		rec.Note = "部分成功"
		return
	}
	rec.Status = "ok"
	if st.Registered == 0 && st.Fail == 0 {
		rec.Note = "无任务完成"
	}
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
