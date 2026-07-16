package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k12reg/internal/config"
)

const scheduleFile = "schedule.json"

// ScheduleConfig is persisted under data/schedule.json.
type ScheduleConfig struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"` // interval | daily
	IntervalMin    int    `json:"interval_minutes"`
	DailyTime      string `json:"daily_time"` // HH:MM local
	Count          *int   `json:"count"`     // nil = use settings total
	SkipIfRunning  bool   `json:"skip_if_running"`
	LastRunAt      string `json:"last_run_at,omitempty"`
	LastRunOK      *bool  `json:"last_run_ok,omitempty"`
	LastRunNote    string `json:"last_run_note,omitempty"`
	NextRunAt      string `json:"next_run_at,omitempty"` // advisory; recomputed live
	FireCount      int    `json:"fire_count"`
}

func defaultSchedule() ScheduleConfig {
	return ScheduleConfig{
		Enabled:       false,
		Mode:          "interval",
		IntervalMin:   60,
		DailyTime:     "09:00",
		SkipIfRunning: true,
	}
}

// Scheduler periodically starts the pipeline according to ScheduleConfig.
type Scheduler struct {
	dataDir string
	runner  *RunManager

	mu     sync.Mutex
	cfg    ScheduleConfig
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewScheduler(dataDir string, runner *RunManager) *Scheduler {
	s := &Scheduler{
		dataDir: dataDir,
		runner:  runner,
		cfg:     loadSchedule(dataDir),
		stopCh:  make(chan struct{}),
	}
	s.recomputeNextLocked()
	s.wg.Add(1)
	go s.loop()
	return s
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Scheduler) Get() ScheduleConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recomputeNextLocked()
	return s.cfg
}

func (s *Scheduler) Update(in ScheduleConfig) (ScheduleConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wasEnabled := s.cfg.Enabled
	cfg := defaultSchedule()
	cfg.Enabled = in.Enabled
	cfg.Mode = in.Mode
	if cfg.Mode != "daily" {
		cfg.Mode = "interval"
	}
	cfg.IntervalMin = in.IntervalMin
	if cfg.IntervalMin < 1 {
		cfg.IntervalMin = 1
	}
	if cfg.IntervalMin > 60*24*7 {
		cfg.IntervalMin = 60 * 24 * 7 // max 7 days
	}
	cfg.DailyTime = stringsTrim(in.DailyTime)
	if cfg.DailyTime == "" {
		cfg.DailyTime = "09:00"
	}
	if _, _, err := parseHHMM(cfg.DailyTime); err != nil {
		return ScheduleConfig{}, fmt.Errorf("daily_time 格式应为 HH:MM，例如 09:30")
	}
	cfg.Count = in.Count
	if cfg.Count != nil && *cfg.Count < 1 {
		return ScheduleConfig{}, fmt.Errorf("count 必须 >= 1")
	}
	cfg.SkipIfRunning = in.SkipIfRunning
	// preserve run stats
	cfg.LastRunAt = s.cfg.LastRunAt
	cfg.LastRunOK = s.cfg.LastRunOK
	cfg.LastRunNote = s.cfg.LastRunNote
	cfg.FireCount = s.cfg.FireCount

	// When turning on interval mode, baseline from now → first fire after one interval.
	if cfg.Enabled && !wasEnabled && cfg.Mode == "interval" {
		cfg.LastRunAt = time.Now().Format(time.RFC3339)
		cfg.LastRunNote = "已启用，等待首个周期"
		ok := true
		cfg.LastRunOK = &ok
	}

	s.cfg = cfg
	s.recomputeNextLocked()
	if err := saveSchedule(s.dataDir, s.cfg); err != nil {
		return ScheduleConfig{}, err
	}
	return s.cfg, nil
}

func (s *Scheduler) loop() {
	defer s.wg.Done()
	// Check frequently so daily times are reasonably accurate.
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if !cfg.Enabled {
		return
	}

	// Determine if we should fire.
	should, reason := s.shouldFire(now, cfg)
	if !should {
		_ = reason
		return
	}

	// Mark fire time before starting to avoid double-fire.
	s.mu.Lock()
	s.cfg.LastRunAt = now.Format(time.RFC3339)
	s.cfg.FireCount++
	s.recomputeNextLocked()
	_ = saveSchedule(s.dataDir, s.cfg)
	s.mu.Unlock()

	var count *int
	reqN := 0
	if cfg.Count != nil && *cfg.Count > 0 {
		c := *cfg.Count
		count = &c
		reqN = c
	}

	// Preview multi-manager plan from current settings (same path as manual run).
	mgrNote := "settings managers"
	if st, err := loadSettingsManagersPreview(s.dataDir, count); err == nil {
		mgrNote = st
	}
	if count != nil {
		s.runner.emit(fmt.Sprintf("⏰ 定时任务触发 · mode=%s · 每母号配额覆盖=%d · %s", cfg.Mode, *count, mgrNote))
	} else {
		s.runner.emit(fmt.Sprintf("⏰ 定时任务触发 · mode=%s · 使用各母号配额 · %s", cfg.Mode, mgrNote))
	}

	if cfg.SkipIfRunning {
		st := s.runner.Status()
		if running, _ := st["running"].(bool); running {
			s.note(false, "跳过：流水线正在运行")
			s.runner.emit("⏰ 定时任务跳过：流水线正在运行")
			now := time.Now().UTC()
			_ = appendTaskRecord(s.dataDir, TaskRecord{
				ID:         newTaskID(now),
				Source:     "schedule",
				Status:     "skipped",
				StartedAt:  now.Format(time.RFC3339),
				FinishedAt: now.Format(time.RFC3339),
				Requested:  reqN,
				Note:       "流水线正在运行",
			})
			return
		}
	}

	// workspaceID "" → runner 使用设置中全部启用母号（多空间）
	if err := s.runner.Start(count, "", "schedule"); err != nil {
		s.note(false, err.Error())
		s.runner.emit("⏰ 定时启动失败: " + err.Error())
		now := time.Now().UTC()
		_ = appendTaskRecord(s.dataDir, TaskRecord{
			ID:         newTaskID(now),
			Source:     "schedule",
			Status:     "error",
			StartedAt:  now.Format(time.RFC3339),
			FinishedAt: now.Format(time.RFC3339),
			Requested:  reqN,
			Note:       err.Error(),
		})
		return
	}
	s.note(true, "已启动 · "+mgrNote)
}

func (s *Scheduler) shouldFire(now time.Time, cfg ScheduleConfig) (bool, string) {
	last, _ := time.Parse(time.RFC3339, cfg.LastRunAt)

	switch cfg.Mode {
	case "daily":
		hh, mm, err := parseHHMM(cfg.DailyTime)
		if err != nil {
			return false, "bad daily_time"
		}
		// Fire once when local clock is at/after target and we haven't fired today.
		target := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		if now.Before(target) {
			return false, "before daily time"
		}
		// Already fired today?
		if !last.IsZero() {
			ly, lm, ld := last.In(now.Location()).Date()
			ny, nm, nd := now.Date()
			if ly == ny && lm == nm && ld == nd {
				return false, "already fired today"
			}
		}
		// Only fire within a 2-minute window after target to avoid late catch-up storms
		// after long downtime — actually better to fire once per day if past target and not yet today.
		return true, "daily"

	default: // interval
		iv := time.Duration(cfg.IntervalMin) * time.Minute
		if iv < time.Minute {
			iv = time.Minute
		}
		if last.IsZero() {
			return false, "no baseline"
		}
		if now.Sub(last) >= iv {
			return true, "interval"
		}
		return false, "waiting"
	}
}

func (s *Scheduler) note(ok bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.LastRunOK = &ok
	s.cfg.LastRunNote = msg
	s.recomputeNextLocked()
	_ = saveSchedule(s.dataDir, s.cfg)
}

func (s *Scheduler) recomputeNextLocked() {
	now := time.Now()
	if !s.cfg.Enabled {
		s.cfg.NextRunAt = ""
		return
	}
	last, _ := time.Parse(time.RFC3339, s.cfg.LastRunAt)

	switch s.cfg.Mode {
	case "daily":
		hh, mm, err := parseHHMM(s.cfg.DailyTime)
		if err != nil {
			s.cfg.NextRunAt = ""
			return
		}
		target := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		if !last.IsZero() {
			ly, lm, ld := last.In(now.Location()).Date()
			ny, nm, nd := now.Date()
			if ly == ny && lm == nm && ld == nd {
				target = target.Add(24 * time.Hour)
			}
		}
		if now.After(target) || now.Equal(target) {
			// if not fired today, next is now (pending); else tomorrow already handled
			if last.IsZero() {
				// show today target or tomorrow if past
				if now.After(target) {
					target = target.Add(24 * time.Hour)
				}
			} else {
				ly, lm, ld := last.In(now.Location()).Date()
				ny, nm, nd := now.Date()
				if !(ly == ny && lm == nm && ld == nd) && !now.Before(target) {
					// pending fire today
					s.cfg.NextRunAt = now.Format(time.RFC3339)
					return
				}
			}
		}
		s.cfg.NextRunAt = target.Format(time.RFC3339)

	default:
		iv := time.Duration(s.cfg.IntervalMin) * time.Minute
		if iv < time.Minute {
			iv = time.Minute
		}
		if last.IsZero() {
			s.cfg.NextRunAt = now.Add(iv).Format(time.RFC3339)
		} else {
			s.cfg.NextRunAt = last.Add(iv).Format(time.RFC3339)
		}
	}
}

// loadSettingsManagersPreview summarizes multi-manager plan for schedule logs.
// count non-nil → each manager quota overridden.
func loadSettingsManagersPreview(dataDir string, count *int) (string, error) {
	cfg, err := config.LoadJSON(filepath.Join(dataDir, settingsFile))
	if err != nil {
		cfg = config.Default()
	}
	cfg.DataDir = dataDir
	mgrs := cfg.ActiveManagers()
	if len(mgrs) == 0 {
		return "no managers", nil
	}
	total := 0
	parts := make([]string, 0, len(mgrs))
	for i, m := range mgrs {
		q := m.Quota
		if count != nil && *count > 0 {
			q = *count
		}
		if q < 1 {
			q = 1
		}
		total += q
		name := strings.TrimSpace(m.SessionFile)
		if name == "" {
			name = truncID(m.WorkspaceID, 8)
		}
		parts = append(parts, fmt.Sprintf("#%d %s×%d", i+1, name, q))
	}
	binding := "shared"
	if cfg.IsPerManagerMail() {
		binding = "per_manager"
	}
	return fmt.Sprintf("managers=%d · %s · total≈%d · %s",
		len(mgrs), binding, total, strings.Join(parts, ", ")), nil
}

func loadSchedule(dataDir string) ScheduleConfig {
	cfg := defaultSchedule()
	b, err := os.ReadFile(filepath.Join(dataDir, scheduleFile))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.Mode != "daily" {
		cfg.Mode = "interval"
	}
	if cfg.IntervalMin < 1 {
		cfg.IntervalMin = 60
	}
	if cfg.DailyTime == "" {
		cfg.DailyTime = "09:00"
	}
	return cfg
}

func saveSchedule(dataDir string, cfg ScheduleConfig) error {
	_ = os.MkdirAll(dataDir, 0o755)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, scheduleFile), append(b, '\n'), 0o644)
}

func parseHHMM(s string) (h, m int, err error) {
	var hh, mm int
	n, e := fmt.Sscanf(s, "%d:%d", &hh, &mm)
	if e != nil || n != 2 || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("invalid time")
	}
	return hh, mm, nil
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
