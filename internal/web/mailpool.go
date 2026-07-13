package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"k12reg/internal/config"
	"k12reg/internal/mail"
)

func (s *Server) handleMailPool(w http.ResponseWriter, r *http.Request) {
	if !s.auth.require(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleMailPoolGet(w, r)
	case http.MethodPost:
		s.handleMailPoolPost(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}

func (s *Server) mailPoolPaths() (mailFile, statePath string, aliasCount int, mailboxesName string, errMsg string) {
	settingsPath := filepath.Join(s.opt.DataDir, "settings.json")
	cfg, err := config.LoadJSON(settingsPath)
	if err != nil {
		cfg = config.Default()
	}
	cfg.DataDir = s.opt.DataDir
	aliasCount = cfg.AliasCount
	if aliasCount < 1 {
		aliasCount = 1
	}
	statePath = filepath.Join(s.opt.DataDir, "outlook_token_state.json")

	name := strings.TrimSpace(cfg.MailboxesFile)
	if name == "" {
		if n := firstMailPoolFile(s.opt.DataDir); n != "" {
			name = n
		}
	}
	mailboxesName = name
	if name == "" {
		return "", statePath, aliasCount, "", "未配置邮箱池文件"
	}
	mailFile = cfg.ResolvePath(name)
	if mailFile == "" {
		mailFile = filepath.Join(s.opt.DataDir, name)
	}
	return mailFile, statePath, aliasCount, mailboxesName, ""
}

func (s *Server) handleMailPoolGet(w http.ResponseWriter, r *http.Request) {
	mailFile, statePath, aliasCount, name, errMsg := s.mailPoolPaths()
	includeSlots := r.URL.Query().Get("slots") == "1" || r.URL.Query().Get("slots") == "true"
	if errMsg != "" {
		writeJSON(w, http.StatusOK, mail.PoolReport{
			MailboxesFile: name,
			StateFile:     "outlook_token_state.json",
			AliasCount:    aliasCount,
			Bases:         []mail.BaseRow{},
			Error:         errMsg,
		})
		return
	}
	rep := mail.BuildPoolReport(mailFile, statePath, aliasCount, includeSlots)
	// Present relative names in UI
	rep.MailboxesFile = name
	rep.StateFile = "outlook_token_state.json"
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleMailPoolPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid json"})
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	switch action {
	case "reset", "reset_base":
		mailFile, statePath, aliasCount, _, errMsg := s.mailPoolPaths()
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": errMsg})
			return
		}
		n, err := mail.ResetBaseState(statePath, body.Email, mailFile, aliasCount)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n, "email": strings.TrimSpace(body.Email)})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "unknown action (use reset)"})
	}
}
