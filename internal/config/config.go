package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the subset needed for the Go CLI (protocol register + K12 join).
type Config struct {
	DataDir string

	// Proxy
	Proxy           string
	ProxiesFile     string
	DefaultProtocol string
	FlareSolverrURL string
	// RegisterProxyRetries: extra proxy rotations on TLS/network register failures.
	RegisterProxyRetries int

	// Registration
	Total   int
	Threads int
	// OAuthPath: register.PathChatGPTWeb | register.PathPlatform (settings: registration.oauth_path).
	OAuthPath string

	// Mail
	MailboxesFile string
	AliasCount    int
	WaitTimeout   float64
	WaitInterval  float64

	// Workspace
	WorkspaceEnabled   bool
	WorkspaceIDs       []string
	// WorkspaceSelectedID is the one used for join/plan (must be in WorkspaceIDs when set).
	// Empty → use WorkspaceIDs[0].
	WorkspaceSelectedID string
	WorkspaceRoute      string
	ApproveRequests     bool
	ManagerSessionFile  string
	ApproveMaxAttempts  int
	// RequireSameDomain: child email domain must match manager (K12 policy).
	RequireSameDomain bool
	// MailBinding: "shared" = one global mail pool (all managers same domain);
	// "per_manager" = each manager may set its own mailboxes_file.
	MailBinding string
	// Managers: multi mother-session slots (quota per workspace). Empty → legacy single manager.
	Managers []ManagerSlot

	// Import APIs (optional; may push to multiple account pools)
	ImportEnabled    bool // true when any endpoint is enabled
	ImportRequireK12 bool // default when endpoint omits require_k12
	// Legacy single-target fields (still filled from first endpoint for compatibility)
	ImportURL      string
	ImportAdminKey string
	// ImportEndpoints is the full list; use ActiveImportEndpoints() at runtime.
	ImportEndpoints []ImportEndpoint

	// Paths for sentinel VM (Node)
	SentinelVMDir string
}

// ImportEndpoint is one account-pool admin API target.
type ImportEndpoint struct {
	Name       string
	Enabled    bool
	URL        string
	AdminKey   string
	RequireK12 bool
}

// ManagerSlot is one mother session / workspace to fill during a batch run.
type ManagerSlot struct {
	Enabled        bool
	SessionFile    string // e.g. session.json / space-a.json
	Quota          int    // accounts to register+join into this workspace
	MailboxesFile  string // optional; used when MailBinding=per_manager
	// Filled at runtime from session JSON (not always persisted):
	WorkspaceID string
	Email       string
	Domain      string
	Label       string // optional display name
}

func Default() Config {
	return Config{
		DataDir:              ".",
		DefaultProtocol:      "socks5",
		RegisterProxyRetries: 3,
		Total:                1,
		Threads:              1,
		MailboxesFile:        "hotmail.txt",
		AliasCount:           1,
		WaitTimeout:          30, // OTP wait (seconds); 30s is enough for Graph delivery
		WaitInterval:         1.5,
		WorkspaceEnabled:     true,
		WorkspaceRoute:       "request",
		ApproveRequests:      true,
		ManagerSessionFile:   "hotsession.json",
		ApproveMaxAttempts:   8, // short backoff; ~12–20s list poll budget
		RequireSameDomain:    true,
		MailBinding:          "shared",
		ImportRequireK12:     true,
		OAuthPath:            "chatgpt_web",
	}
}

// LoadJSON loads overlay settings (same shape as webapp settings.json).
func LoadJSON(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return cfg, err
	}
	cfg.ApplyMap(raw)
	return cfg, nil
}

// ApplyMap merges a loose JSON map into cfg.
func (c *Config) ApplyMap(raw map[string]any) {
	if m, ok := raw["proxy"].(map[string]any); ok {
		if v, ok := m["url"].(string); ok && strings.TrimSpace(v) != "" {
			c.Proxy = strings.TrimSpace(v)
		}
		if v, ok := m["proxies_file"].(string); ok {
			c.ProxiesFile = strings.TrimSpace(v)
		}
		if v, ok := m["default_protocol"].(string); ok && v != "" {
			c.DefaultProtocol = v
		}
		if v, ok := m["flaresolverr_url"].(string); ok {
			c.FlareSolverrURL = strings.TrimSpace(v)
		}
	}
	if m, ok := raw["registration"].(map[string]any); ok {
		if v, ok := asInt(m["total"]); ok && v > 0 {
			c.Total = v
		}
		if v, ok := asInt(m["threads"]); ok && v > 0 {
			c.Threads = v
		}
		// oauth_path | auth_path | path aliases
		if v, ok := m["oauth_path"].(string); ok && strings.TrimSpace(v) != "" {
			c.OAuthPath = strings.TrimSpace(v)
		} else if v, ok := m["auth_path"].(string); ok && strings.TrimSpace(v) != "" {
			c.OAuthPath = strings.TrimSpace(v)
		} else if v, ok := m["path"].(string); ok && strings.TrimSpace(v) != "" {
			c.OAuthPath = strings.TrimSpace(v)
		}
	}
	if m, ok := raw["mail"].(map[string]any); ok {
		if v, ok := asFloat(m["wait_timeout"]); ok && v > 0 {
			c.WaitTimeout = v
		}
		if v, ok := asFloat(m["wait_interval"]); ok && v > 0 {
			c.WaitInterval = v
		}
		if providers, ok := m["providers"].([]any); ok {
			for _, p := range providers {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := pm["type"].(string); t != "" && t != "outlook_token" {
					continue
				}
				if v, ok := pm["mailboxes_file"].(string); ok && v != "" {
					c.MailboxesFile = v
				}
				if v, ok := asInt(pm["alias_count"]); ok && v > 0 {
					c.AliasCount = v
				}
			}
		}
		if v, ok := m["mailboxes_file"].(string); ok && v != "" {
			c.MailboxesFile = v
		}
		// Top-level mail.alias_count (settings form)
		if v, ok := asInt(m["alias_count"]); ok && v > 0 {
			c.AliasCount = v
		}
	}
	if m, ok := raw["workspace"].(map[string]any); ok {
		if v, ok := m["enabled"].(bool); ok {
			c.WorkspaceEnabled = v
		}
		if v, ok := m["route"].(string); ok && v != "" {
			c.WorkspaceRoute = v
		}
		if v, ok := m["approve_requests"].(bool); ok {
			c.ApproveRequests = v
		}
		if v, ok := m["manager_session_file"].(string); ok && v != "" {
			c.ManagerSessionFile = v
		}
		if v, ok := asInt(m["approve_max_attempts"]); ok && v > 0 {
			c.ApproveMaxAttempts = v
		}
		if v, ok := m["require_same_domain"].(bool); ok {
			c.RequireSameDomain = v
		}
		if v, ok := m["mail_binding"].(string); ok && strings.TrimSpace(v) != "" {
			c.MailBinding = strings.ToLower(strings.TrimSpace(v))
		}
		if ids, ok := m["ids"].([]any); ok {
			c.WorkspaceIDs = nil
			for _, id := range ids {
				if s, ok := id.(string); ok && strings.TrimSpace(s) != "" {
					c.WorkspaceIDs = append(c.WorkspaceIDs, strings.TrimSpace(s))
				}
			}
		}
		// Also accept single string "id" for convenience.
		if v, ok := m["id"].(string); ok && strings.TrimSpace(v) != "" {
			id := strings.TrimSpace(v)
			if len(c.WorkspaceIDs) == 0 {
				c.WorkspaceIDs = []string{id}
			}
			c.WorkspaceSelectedID = id
		}
		if v, ok := m["selected_id"].(string); ok {
			c.WorkspaceSelectedID = strings.TrimSpace(v)
		}
		if rawMgrs, ok := m["managers"].([]any); ok {
			c.Managers = nil
			for _, item := range rawMgrs {
				em, ok := item.(map[string]any)
				if !ok {
					continue
				}
				slot := ManagerSlot{
					Enabled:       true,
					SessionFile:   strings.TrimSpace(asStringAny(em["session_file"])),
					MailboxesFile: strings.TrimSpace(asStringAny(em["mailboxes_file"])),
					WorkspaceID:   strings.TrimSpace(asStringAny(em["workspace_id"])),
					Email:         strings.TrimSpace(asStringAny(em["email"])),
					Domain:        strings.TrimSpace(asStringAny(em["domain"])),
					Label:         strings.TrimSpace(asStringAny(em["label"])),
					Quota:         20,
				}
				if v, ok := em["enabled"].(bool); ok {
					slot.Enabled = v
				}
				if v, ok := asInt(em["quota"]); ok && v > 0 {
					slot.Quota = v
				}
				// Accept manager_session_file as alias
				if slot.SessionFile == "" {
					slot.SessionFile = strings.TrimSpace(asStringAny(em["manager_session_file"]))
				}
				if slot.SessionFile == "" && slot.WorkspaceID == "" {
					continue
				}
				if slot.Domain == "" && slot.Email != "" {
					slot.Domain = EmailDomain(slot.Email)
				}
				c.Managers = append(c.Managers, slot)
			}
		}
	}
	if m, ok := raw["import_api"].(map[string]any); ok {
		if v, ok := m["require_k12"].(bool); ok {
			c.ImportRequireK12 = v
		}
		// New multi-endpoint shape: import_api.endpoints[]
		if rawEps, ok := m["endpoints"].([]any); ok && len(rawEps) > 0 {
			c.ImportEndpoints = nil
			for i, item := range rawEps {
				em, ok := item.(map[string]any)
				if !ok {
					continue
				}
				ep := ImportEndpoint{
					Name:       strings.TrimSpace(asStringAny(em["name"])),
					Enabled:    true,
					URL:        strings.TrimSpace(asStringAny(em["url"])),
					AdminKey:   strings.TrimSpace(asStringAny(em["admin_key"])),
					RequireK12: c.ImportRequireK12,
				}
				if v, ok := em["enabled"].(bool); ok {
					ep.Enabled = v
				}
				if v, ok := em["require_k12"].(bool); ok {
					ep.RequireK12 = v
				}
				if ep.Name == "" {
					ep.Name = fmt.Sprintf("api-%d", i+1)
				}
				if ep.URL == "" {
					continue
				}
				c.ImportEndpoints = append(c.ImportEndpoints, ep)
			}
		} else {
			// Legacy single object: import_api.url / admin_key / enabled
			url := strings.TrimSpace(asStringAny(m["url"]))
			key := strings.TrimSpace(asStringAny(m["admin_key"]))
			en := true
			if v, ok := m["enabled"].(bool); ok {
				en = v
			}
			if url != "" {
				c.ImportEndpoints = []ImportEndpoint{{
					Name:       "default",
					Enabled:    en,
					URL:        url,
					AdminKey:   key,
					RequireK12: c.ImportRequireK12,
				}}
			}
		}
		// Sync legacy flags from active list
		c.syncImportLegacy()
	}
	// Also accept top-level import_apis array
	if rawEps, ok := raw["import_apis"].([]any); ok && len(rawEps) > 0 {
		c.ImportEndpoints = nil
		for i, item := range rawEps {
			em, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ep := ImportEndpoint{
				Name:       strings.TrimSpace(asStringAny(em["name"])),
				Enabled:    true,
				URL:        strings.TrimSpace(asStringAny(em["url"])),
				AdminKey:   strings.TrimSpace(asStringAny(em["admin_key"])),
				RequireK12: c.ImportRequireK12,
			}
			if v, ok := em["enabled"].(bool); ok {
				ep.Enabled = v
			}
			if v, ok := em["require_k12"].(bool); ok {
				ep.RequireK12 = v
			}
			if ep.Name == "" {
				ep.Name = fmt.Sprintf("api-%d", i+1)
			}
			if ep.URL == "" {
				continue
			}
			c.ImportEndpoints = append(c.ImportEndpoints, ep)
		}
		c.syncImportLegacy()
	}
}

func asStringAny(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func (c *Config) syncImportLegacy() {
	c.ImportEnabled = false
	c.ImportURL = ""
	c.ImportAdminKey = ""
	for _, ep := range c.ImportEndpoints {
		if !ep.Enabled || ep.URL == "" {
			continue
		}
		if !c.ImportEnabled {
			c.ImportEnabled = true
			c.ImportURL = ep.URL
			c.ImportAdminKey = ep.AdminKey
		}
	}
}

// ActiveImportEndpoints returns enabled endpoints with non-empty URL.
func (c Config) ActiveImportEndpoints() []ImportEndpoint {
	var out []ImportEndpoint
	for _, ep := range c.ImportEndpoints {
		if ep.Enabled && strings.TrimSpace(ep.URL) != "" {
			out = append(out, ep)
		}
	}
	return out
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case json.Number:
		i, err := t.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}

// ResolvePath joins relative paths against DataDir.
func (c Config) ResolvePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.DataDir, p)
}

// LoadProxies reads a proxy list file (one URL per line).
func LoadProxies(path, defaultProto string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "://") {
			proto := defaultProto
			if proto == "" {
				proto = "socks5"
			}
			line = proto + "://" + line
		}
		out = append(out, line)
	}
	return out, nil
}

// ActiveWorkspaceID returns the workspace used for join/plan checks.
// Prefer selected_id (from manager session account.id); fall back to first ids entry.
func (c Config) ActiveWorkspaceID() string {
	sel := strings.TrimSpace(c.WorkspaceSelectedID)
	if sel != "" {
		return sel
	}
	if len(c.WorkspaceIDs) > 0 {
		return c.WorkspaceIDs[0]
	}
	// Multi-manager: first enabled slot with workspace id
	for _, m := range c.Managers {
		if m.Enabled && strings.TrimSpace(m.WorkspaceID) != "" {
			return strings.TrimSpace(m.WorkspaceID)
		}
	}
	return ""
}

// ActiveWorkspaceIDs returns a one-element slice for join/plan APIs that take []string.
func (c Config) ActiveWorkspaceIDs() []string {
	id := c.ActiveWorkspaceID()
	if id == "" {
		return nil
	}
	return []string{id}
}

// ActiveManagers returns enabled manager slots. Falls back to legacy single
// manager_session_file + selected_id when managers[] is empty.
func (c Config) ActiveManagers() []ManagerSlot {
	var out []ManagerSlot
	for _, m := range c.Managers {
		if !m.Enabled {
			continue
		}
		if strings.TrimSpace(m.SessionFile) == "" && strings.TrimSpace(m.WorkspaceID) == "" {
			continue
		}
		if m.Quota < 1 {
			m.Quota = 1
		}
		out = append(out, m)
	}
	if len(out) > 0 {
		return out
	}
	// Legacy single-manager shape
	sf := strings.TrimSpace(c.ManagerSessionFile)
	wid := strings.TrimSpace(c.WorkspaceSelectedID)
	if wid == "" && len(c.WorkspaceIDs) > 0 {
		wid = c.WorkspaceIDs[0]
	}
	if sf == "" && wid == "" {
		return nil
	}
	q := c.Total
	if q < 1 {
		q = 1
	}
	return []ManagerSlot{{
		Enabled:       true,
		SessionFile:   sf,
		Quota:         q,
		MailboxesFile: "",
		WorkspaceID:   wid,
	}}
}

// IsPerManagerMail reports whether each manager uses its own mailboxes file.
func (c Config) IsPerManagerMail() bool {
	b := strings.ToLower(strings.TrimSpace(c.MailBinding))
	return b == "per_manager" || b == "per-manager" || b == "bound"
}

func (c Config) Validate() error {
	if c.Total < 1 {
		return fmt.Errorf("total must be >= 1")
	}
	if c.WorkspaceEnabled {
		mgrs := c.ActiveManagers()
		if len(mgrs) == 0 && c.ActiveWorkspaceID() == "" {
			return fmt.Errorf("workspace.enabled but no manager session configured")
		}
	}
	for _, ep := range c.ActiveImportEndpoints() {
		if ep.AdminKey == "" {
			return fmt.Errorf("import_api endpoint %q requires admin_key", ep.Name)
		}
	}
	return nil
}

// EmailDomain returns the lowercased domain part of an email.
func EmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	_, dom, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return strings.TrimSpace(dom)
}

// SameEmailDomain reports whether two addresses share a domain.
func SameEmailDomain(a, b string) bool {
	da, db := EmailDomain(a), EmailDomain(b)
	return da != "" && da == db
}
