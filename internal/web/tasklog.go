package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const taskHistoryFile = "task_history.jsonl"
const taskHistoryMax = 300 // keep last N records on disk

// TaskRecord is one pipeline run (manual or scheduled).
type TaskRecord struct {
	ID            string  `json:"id"`
	Source        string  `json:"source"` // manual | schedule
	Status        string  `json:"status"` // ok | fail | cancelled | error | skipped
	StartedAt     string  `json:"started_at"`
	FinishedAt    string  `json:"finished_at"`
	ElapsedSec    float64 `json:"elapsed_sec"`
	Requested     int     `json:"requested"`
	Registered    int64   `json:"registered"`
	Fail          int64   `json:"fail"`
	JoinOK        int64   `json:"join_ok"`
	ApproveOK     int64   `json:"approve_ok"`
	K12           int64   `json:"k12"`
	ImportOK      int64   `json:"import_ok"`
	ExitCode      int     `json:"exit_code"`
	WorkspaceID   string  `json:"workspace_id,omitempty"`
	MailboxesFile string  `json:"mailboxes_file,omitempty"`
	Threads       int     `json:"threads,omitempty"`
	Note          string  `json:"note,omitempty"`
}

var taskLogMu sync.Mutex

func taskHistoryPath(dataDir string) string {
	return filepath.Join(dataDir, taskHistoryFile)
}

func newTaskID(t time.Time) string {
	return t.UTC().Format("20060102T150405.000") + "-" + strconv.FormatInt(t.UnixNano()%1000, 10)
}

func appendTaskRecord(dataDir string, rec TaskRecord) error {
	taskLogMu.Lock()
	defer taskLogMu.Unlock()

	if rec.ID == "" {
		rec.ID = newTaskID(time.Now())
	}
	_ = os.MkdirAll(dataDir, 0o755)
	path := taskHistoryPath(dataDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	// Best-effort trim (async-ish: rewrite if file got huge).
	if st, e := f.Stat(); e == nil && st.Size() > 2*1024*1024 {
		_ = trimTaskHistoryLocked(path, taskHistoryMax)
	}
	return nil
}

func loadTaskRecords(dataDir string, limit int) ([]TaskRecord, error) {
	taskLogMu.Lock()
	defer taskLogMu.Unlock()

	path := taskHistoryPath(dataDir)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskRecord{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var all []TaskRecord
	sc := bufio.NewScanner(f)
	// large lines unlikely
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec TaskRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		all = append(all, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Newest first
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].StartedAt > all[j].StartedAt
	})
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	if limit < len(all) {
		all = all[:limit]
	}
	return all, nil
}

func clearTaskRecords(dataDir string) error {
	taskLogMu.Lock()
	defer taskLogMu.Unlock()
	path := taskHistoryPath(dataDir)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func trimTaskHistoryLocked(path string, keep int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	_ = f.Close()
	if len(lines) <= keep {
		return nil
	}
	lines = lines[len(lines)-keep:]
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, ln := range lines {
		if _, err := fmt.Fprintln(out, ln); err != nil {
			_ = out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func taskSummary(records []TaskRecord) map[string]any {
	var reg, fail, okN, cancelN, errN int64
	var elapsed float64
	for _, r := range records {
		reg += r.Registered
		fail += r.Fail
		elapsed += r.ElapsedSec
		switch r.Status {
		case "ok":
			okN++
		case "cancelled":
			cancelN++
		case "error", "fail":
			errN++
		}
	}
	return map[string]any{
		"runs":             len(records),
		"runs_ok":          okN,
		"runs_fail":        errN,
		"runs_cancelled":   cancelN,
		"total_registered": reg,
		"total_fail":       fail,
		"total_elapsed_sec": elapsed,
	}
}
