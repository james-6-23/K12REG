package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var mu sync.Mutex

// Account is one registered row.
type Account map[string]any

const (
	// AccountsJSONL is the primary append-only store (one compact JSON object per line).
	AccountsJSONL = "registered_accounts.jsonl"
	// AccountsJSON is the legacy array file; migrated once into JSONL.
	AccountsJSON = "registered_accounts.json"
)

// AccountsFile returns the path used for append / load (JSONL).
func AccountsFile(dataDir string) string {
	return filepath.Join(dataDir, AccountsJSONL)
}

// AppendAccount appends one account as a single JSONL line (O(1), no full rewrite).
func AppendAccount(path string, acc Account) error {
	mu.Lock()
	defer mu.Unlock()

	// If caller still passes .../registered_accounts.json, write to sibling .jsonl.
	path = normalizeAccountsPath(path)
	if err := ensureMigrated(filepath.Dir(path)); err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	// Compact single-line JSON (no indent) keeps file size down.
	b, err := json.Marshal(acc)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func normalizeAccountsPath(path string) string {
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	if base == AccountsJSON || base == AccountsJSONL {
		return filepath.Join(dir, AccountsJSONL)
	}
	// custom path: if ends with .json prefer .jsonl sibling for append
	if filepath.Ext(path) == ".json" {
		return path[:len(path)-5] + ".jsonl"
	}
	return path
}

// ensureMigrated converts legacy registered_accounts.json → .jsonl once.
func ensureMigrated(dataDir string) error {
	jsonl := filepath.Join(dataDir, AccountsJSONL)
	if st, err := os.Stat(jsonl); err == nil && st.Size() > 0 {
		return nil
	}
	legacy := filepath.Join(dataDir, AccountsJSON)
	b, err := os.ReadFile(legacy)
	if err != nil {
		return nil // nothing to migrate
	}
	var list []Account
	if json.Unmarshal(b, &list) != nil || len(list) == 0 {
		return nil
	}
	_ = os.MkdirAll(dataDir, 0o755)
	f, err := os.OpenFile(jsonl, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, acc := range list {
		line, e := json.Marshal(acc)
		if e != nil {
			_ = f.Close()
			return e
		}
		if _, e = w.Write(line); e != nil {
			_ = f.Close()
			return e
		}
		if e = w.WriteByte('\n'); e != nil {
			_ = f.Close()
			return e
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Keep legacy as backup so downloads still work; mark renamed.
	_ = os.Rename(legacy, legacy+".bak")
	return nil
}

// AccountSummary is a lightweight row for the results table (no tokens).
type AccountSummary struct {
	Email             any `json:"email"`
	PlanType          any `json:"plan_type"`
	JoinStatus        any `json:"join_status"`
	ApproveStatus     any `json:"approve_status"`
	ElevateStatus     any `json:"elevate_status"`
	ImportStatus      any `json:"import_status"`
	ChatGPTAccountID  any `json:"chatgpt_account_id"`
	HasAccessToken    bool `json:"has_access_token"`
	HasRefreshToken   bool `json:"has_refresh_token"`
}

// LoadAccountPage reads account summaries with pagination.
// order=desc returns newest first (default); order=asc oldest first.
func LoadAccountPage(dataDir string, offset, limit int, orderDesc bool) (rows []AccountSummary, total int, err error) {
	mu.Lock()
	defer mu.Unlock()
	_ = ensureMigrated(dataDir)

	path := filepath.Join(dataDir, AccountsJSONL)
	// Fall back to legacy JSON if jsonl still missing
	if _, e := os.Stat(path); e != nil {
		return loadFromLegacyJSON(dataDir, offset, limit, orderDesc)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil
	}
	defer f.Close()

	// Stream all summaries (tokens discarded while parsing) — still cheaper than
	// loading pretty-printed multi-MB JSON into memory as full maps with tokens.
	var all []AccountSummary
	sc := bufio.NewScanner(f)
	// large tokens may exceed default 64K buffer
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			continue
		}
		all = append(all, summaryFromMap(raw))
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	total = len(all)
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	// order
	var ordered []AccountSummary
	if orderDesc {
		ordered = make([]AccountSummary, total)
		for i, a := range all {
			ordered[total-1-i] = a
		}
	} else {
		ordered = all
	}
	if offset >= total {
		return []AccountSummary{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return ordered[offset:end], total, nil
}

func loadFromLegacyJSON(dataDir string, offset, limit int, orderDesc bool) ([]AccountSummary, int, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, AccountsJSON))
	if err != nil {
		return []AccountSummary{}, 0, nil
	}
	var list []map[string]any
	if json.Unmarshal(b, &list) != nil {
		return []AccountSummary{}, 0, nil
	}
	total := len(list)
	var ordered []AccountSummary
	if orderDesc {
		for i := total - 1; i >= 0; i-- {
			ordered = append(ordered, summaryFromMap(list[i]))
		}
	} else {
		for _, a := range list {
			ordered = append(ordered, summaryFromMap(a))
		}
	}
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []AccountSummary{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return ordered[offset:end], total, nil
}

func summaryFromMap(a map[string]any) AccountSummary {
	return AccountSummary{
		Email:            a["email"],
		PlanType:         a["plan_type"],
		JoinStatus:       a["join_status"],
		ApproveStatus:    a["approve_status"],
		ElevateStatus:    a["elevate_status"],
		ImportStatus:     a["import_status"],
		ChatGPTAccountID: a["chatgpt_account_id"],
		HasAccessToken:   strNonEmpty(a["access_token"]),
		HasRefreshToken:  strNonEmpty(a["refresh_token"]),
	}
}

func strNonEmpty(v any) bool {
	s, _ := v.(string)
	return s != ""
}

// CountAccounts returns total registered rows.
func CountAccounts(dataDir string) int {
	rows, total, _ := LoadAccountPage(dataDir, 0, 1, true)
	_ = rows
	return total
}

// ExportAccountsJSON writes a compact JSON array for download (streaming).
func ExportAccountsJSON(dataDir, dest string) error {
	mu.Lock()
	defer mu.Unlock()
	_ = ensureMigrated(dataDir)
	src := filepath.Join(dataDir, AccountsJSONL)
	f, err := os.Open(src)
	if err != nil {
		// try legacy
		b, e := os.ReadFile(filepath.Join(dataDir, AccountsJSON))
		if e != nil {
			return fmt.Errorf("no accounts file")
		}
		return os.WriteFile(dest, b, 0o644)
	}
	defer f.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.WriteString("[\n"); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 8*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !first {
			if _, err := out.WriteString(",\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := out.Write(line); err != nil {
			return err
		}
	}
	if _, err := out.WriteString("\n]\n"); err != nil {
		return err
	}
	return sc.Err()
}

func AppendAccessToken(path, token string) error {
	if token == "" {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(token + "\n")
	return err
}
