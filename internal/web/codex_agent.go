package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"k12reg/internal/codexagent"
	"k12reg/internal/config"
	"k12reg/internal/importapi"
	"k12reg/internal/storage"
)

// handleCodexAgent:
//
//	POST /api/codex-agent
//	  body: { "access_token": "eyJ..." }  — single token
//	     or { "tokens": ["eyJ...", ...] }
//	     or { "from_access_token_file": true }
//	     or { "from_accounts": true, "limit": 50 }
//	  optional: { "verify_task": true, "proxy": "socks5://...", "import": true }
//
//	POST /api/codex-agent/import
//	  Push existing data/codex_auth/*.json to agent_identity import endpoints
//	  (codex2api POST .../codex/agent-identity/import). See AGENT_IDENTITY_IMPORT.md.
//
//	GET  /api/codex-agent — list generated files under codex_auth/
func (s *Server) handleCodexAgent(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	// Sub-path: /api/codex-agent/import
	if strings.HasSuffix(r.URL.Path, "/import") || strings.HasSuffix(r.URL.Path, "/import/") {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
			return
		}
		s.importCodexAuthToEndpoints(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listCodexAuthFiles(w)
	case http.MethodPost:
		s.runCodexAgent(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) listCodexAuthFiles(w http.ResponseWriter) {
	cfg := s.loadCodexConfig()
	dir := filepath.Join(s.opt.DataDir, cfg.CodexAgentOutputDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"output_dir": cfg.CodexAgentOutputDir,
			"files":      []any{},
			"count":      0,
		})
		return
	}
	type fileInfo struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") && !strings.HasSuffix(strings.ToLower(name), ".jsonl") {
			continue
		}
		info, _ := e.Info()
		var sz int64
		if info != nil {
			sz = info.Size()
		}
		files = append(files, fileInfo{Name: name, Size: sz})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output_dir": cfg.CodexAgentOutputDir,
		"files":      files,
		"count":      len(files),
	})
}

type codexAgentReq struct {
	AccessToken         string   `json:"access_token"`
	Tokens              []string `json:"tokens"`
	FromAccessTokenFile bool     `json:"from_access_token_file"`
	FromAccounts        bool     `json:"from_accounts"`
	Limit               int      `json:"limit"`
	VerifyTask          *bool    `json:"verify_task"`
	Proxy               string   `json:"proxy"`
	Threads             int      `json:"threads"`
	// Import after register: push new auth.json to agent_identity endpoints.
	Import bool `json:"import"`
}

func (s *Server) runCodexAgent(w http.ResponseWriter, r *http.Request) {
	var req codexAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid json: " + err.Error()})
		return
	}

	cfg := s.loadCodexConfig()
	verify := cfg.CodexAgentVerifyTask
	if req.VerifyTask != nil {
		verify = *req.VerifyTask
	}

	tokens := collectTokens(s.opt.DataDir, req)
	if len(tokens) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"detail": "no access_token provided (use access_token / tokens / from_access_token_file / from_accounts)",
		})
		return
	}
	if req.Limit > 0 && len(tokens) > req.Limit {
		tokens = tokens[:req.Limit]
	}

	threads := req.Threads
	if threads < 1 {
		threads = 3
	}
	if threads > 16 {
		threads = 16
	}

	outDir := filepath.Join(s.opt.DataDir, cfg.CodexAgentOutputDir)
	type result struct {
		Email          string `json:"email,omitempty"`
		AccountID      string `json:"account_id,omitempty"`
		AgentRuntimeID string `json:"agent_runtime_id,omitempty"`
		File           string `json:"file,omitempty"`
		OK             bool   `json:"ok"`
		Error          string `json:"error,omitempty"`
	}

	results := make([]result, len(tokens))
	var okN, failN atomic.Int64
	jobs := make(chan int, len(tokens))
	for i := range tokens {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for t := 0; t < threads; t++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				tok := strings.TrimSpace(tokens[i])
				if tok == "" {
					results[i] = result{OK: false, Error: "empty token"}
					failN.Add(1)
					continue
				}
				auth, err := codexagent.Create(codexagent.Options{
					AccessToken: tok,
					Proxy:       strings.TrimSpace(req.Proxy),
					VerifyTask:  verify,
				})
				if err != nil {
					results[i] = result{OK: false, Error: err.Error()}
					failN.Add(1)
					continue
				}
				base := codexagent.SafeFilename(auth.AgentIdentity.Email)
				if base == "unknown" {
					base = codexagent.SafeFilename(auth.AgentIdentity.AccountID)
				}
				rel := filepath.ToSlash(filepath.Join(cfg.CodexAgentOutputDir, base+".json"))
				path := filepath.Join(outDir, base+".json")
				if err := codexagent.WriteAuthJSON(path, auth); err != nil {
					results[i] = result{OK: false, Error: err.Error()}
					failN.Add(1)
					continue
				}
				_ = codexagent.AppendAuthJSONL(filepath.Join(outDir, "agents.jsonl"), auth)
				results[i] = result{
					OK:             true,
					Email:          auth.AgentIdentity.Email,
					AccountID:      auth.AgentIdentity.AccountID,
					AgentRuntimeID: auth.AgentIdentity.AgentRuntimeID,
					File:           rel,
				}
				okN.Add(1)
			}
		}()
	}
	wg.Wait()

	out := map[string]any{
		"ok":         true,
		"total":      len(tokens),
		"success":    okN.Load(),
		"failed":     failN.Load(),
		"output_dir": cfg.CodexAgentOutputDir,
		"results":    results,
	}
	if req.Import && okN.Load() > 0 {
		// Collect successfully written auth files and batch-import.
		var files []string
		for _, r := range results {
			if !r.OK || r.File == "" {
				continue
			}
			p := filepath.Join(s.opt.DataDir, filepath.FromSlash(r.File))
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			files = append(files, string(b))
		}
		out["import"] = s.pushAgentIdentityFiles(cfg, files)
	}
	writeJSON(w, http.StatusOK, out)
}

// importCodexAuthToEndpoints reads data/codex_auth/*.json and batch-imports
// to all enabled import endpoints with mode=agent_identity.
func (s *Server) importCodexAuthToEndpoints(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Optional explicit file basenames under codex_auth/; empty = all *.json
		Files []string `json:"files"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	cfg := s.loadCodexConfig()
	dir := filepath.Join(s.opt.DataDir, cfg.CodexAgentOutputDir)
	var files []string
	if len(req.Files) > 0 {
		for _, name := range req.Files {
			name = filepath.Base(strings.TrimSpace(name))
			if name == "" || name == "agents.jsonl" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			files = append(files, string(b))
		}
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"detail": "no codex_auth dir: " + err.Error(),
			})
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				continue
			}
			if name == "agents.jsonl" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			// Skip non-agent files (rough check)
			if !strings.Contains(string(b), "agent_identity") && !strings.Contains(string(b), "agentIdentity") {
				continue
			}
			files = append(files, string(b))
		}
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"detail": "no auth.json files under " + cfg.CodexAgentOutputDir,
		})
		return
	}

	imp := s.pushAgentIdentityFiles(cfg, files)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"file_count": len(files),
		"output_dir": cfg.CodexAgentOutputDir,
		"import":     imp,
	})
}

// pushAgentIdentityFiles posts to every agent_identity import endpoint (batch API).
func (s *Server) pushAgentIdentityFiles(cfg config.Config, files []string) map[string]any {
	eps := cfg.ActiveImportEndpoints()
	type epOut struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Mode     string `json:"mode"`
		OK       bool   `json:"ok"`
		Total    int    `json:"total,omitempty"`
		Imported int    `json:"imported,omitempty"`
		Failed   int    `json:"failed,omitempty"`
		Error    string `json:"error,omitempty"`
		Items    any    `json:"items,omitempty"`
	}
	var outs []epOut
	agentN := 0
	for _, ep := range eps {
		mode := importapi.NormalizeMode(ep.Mode)
		if mode != importapi.ModeAgentIdentity {
			continue
		}
		agentN++
		label := ep.Name
		if label == "" {
			label = ep.URL
		}
		br := importapi.PushAgentIdentityBatchAll(ep.URL, ep.AdminKey, files, ep.ProxyURL, "")
		o := epOut{
			Name: label, URL: ep.URL, Mode: mode,
			OK: br.OK, Total: br.Total, Imported: br.Imported, Failed: br.Failed,
			Error: br.Error, Items: br.Items,
		}
		outs = append(outs, o)
	}
	if agentN == 0 {
		return map[string]any{
			"ok":      false,
			"error":   "no enabled import endpoints with mode=agent_identity",
			"hint":    "设置 → 导入 API → 模式选「Agent Identity」",
			"results": outs,
		}
	}
	allOK := true
	for _, o := range outs {
		if !o.OK {
			allOK = false
			break
		}
	}
	return map[string]any{
		"ok":      allOK,
		"results": outs,
	}
}

func collectTokens(dataDir string, req codexAgentReq) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		// Skip comment lines from files
		if strings.HasPrefix(t, "#") {
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	add(req.AccessToken)
	for _, t := range req.Tokens {
		add(t)
	}

	if req.FromAccessTokenFile {
		path := filepath.Join(dataDir, "access_token.txt")
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				add(sc.Text())
			}
			_ = f.Close()
		}
	}

	if req.FromAccounts {
		// Stream accounts with access_token from JSONL.
		path := storage.AccountsFile(dataDir)
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
			for sc.Scan() {
				line := sc.Bytes()
				if len(line) == 0 {
					continue
				}
				var raw map[string]any
				if json.Unmarshal(line, &raw) != nil {
					continue
				}
				if t, _ := raw["access_token"].(string); t != "" {
					add(t)
				}
			}
			_ = f.Close()
		}
	}
	return out
}

func (s *Server) loadCodexConfig() config.Config {
	cfg := config.Default()
	cfg.DataDir = s.opt.DataDir
	sp := filepath.Join(s.opt.DataDir, settingsFile)
	if loaded, err := config.LoadJSON(sp); err == nil {
		loaded.DataDir = s.opt.DataDir
		cfg = loaded
	}
	if strings.TrimSpace(cfg.CodexAgentOutputDir) == "" {
		cfg.CodexAgentOutputDir = "codex_auth"
	}
	// Sanitize output dir to a single path segment under data.
	cfg.CodexAgentOutputDir = filepath.Base(cfg.CodexAgentOutputDir)
	if cfg.CodexAgentOutputDir == "." || cfg.CodexAgentOutputDir == ".." {
		cfg.CodexAgentOutputDir = "codex_auth"
	}
	return cfg
}
