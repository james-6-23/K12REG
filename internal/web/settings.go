package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const settingsFile = "settings.json"

// curated form shape returned by GET /api/settings
func getEffectiveSettings(dataDir string) map[string]any {
	ov := loadOverlay(dataDir)
	// defaults matching Python form
	out := map[string]any{
		"import_api": map[string]any{
			"require_k12": true,
			"endpoints":   []any{},
		},
		"registration": map[string]any{
			"total":         1,
			"threads":       1,
			"mode":          "protocol",
			"pipeline_gate": "reg",
			// oauth_path: chatgpt_web (default) | platform (legacy app_2SK)
			"oauth_path": "chatgpt_web",
		},
		"workspace": map[string]any{
			"enabled":              true,
			"ids":                  []any{},
			"selected_id":          "",
			"manager_session_file": "session.json",
			"approve_requests":     true,
			"mail_binding":         "shared",
			"managers":             []any{},
		},
		"proxy": map[string]any{
			"proxies_file":     "",
			"default_protocol": "socks5",
			"flaresolverr_url": "",
		},
		"mail": map[string]any{
			"mailboxes_file": "",
			"alias_count":    1,
			"wait_timeout":   30,
			"wait_interval":  1.5,
		},
		// Codex Agent Identity: after live AT, register agent → data/codex_auth/
		"codex_agent": map[string]any{
			"enabled":     false,
			"verify_task": true,
			"output_dir":  "codex_auth",
		},
	}
	deepMerge(out, extractCurated(ov))
	// mailboxes_file / alias_count may live under mail.providers[0]
	if mail, ok := ov["mail"].(map[string]any); ok {
		mm := out["mail"].(map[string]any)
		if mf, ok := mail["mailboxes_file"].(string); ok && mf != "" {
			mm["mailboxes_file"] = mf
		}
		if ac, ok := asIntVal(mail["alias_count"]); ok && ac > 0 {
			mm["alias_count"] = ac
		}
		if wt, ok := asFloatVal(mail["wait_timeout"]); ok && wt > 0 {
			mm["wait_timeout"] = wt
		}
		if wi, ok := asFloatVal(mail["wait_interval"]); ok && wi > 0 {
			mm["wait_interval"] = wi
		}
		if providers, ok := mail["providers"].([]any); ok && len(providers) > 0 {
			if p0, ok := providers[0].(map[string]any); ok {
				if mf, ok := p0["mailboxes_file"].(string); ok && mf != "" {
					mm["mailboxes_file"] = mf
				}
				if ac, ok := asIntVal(p0["alias_count"]); ok && ac > 0 {
					mm["alias_count"] = ac
				}
			}
		}
		out["mail"] = mm
	}
	return out
}

func extractCurated(ov map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := ov["import_api"].(map[string]any); ok {
		out["import_api"] = normalizeImportAPI(m)
	}
	if m, ok := ov["codex_agent"].(map[string]any); ok {
		outDir := orStr(asString(m["output_dir"]), "codex_auth")
		if outDir == "" {
			outDir = "codex_auth"
		}
		verify := true
		if v, ok := m["verify_task"].(bool); ok {
			verify = v
		}
		out["codex_agent"] = map[string]any{
			"enabled":     m["enabled"] == true,
			"verify_task": verify,
			"output_dir":  outDir,
		}
	}
	if m, ok := ov["registration"].(map[string]any); ok {
		oauthPath := orStr(asString(m["oauth_path"]), "")
		if oauthPath == "" {
			oauthPath = orStr(asString(m["auth_path"]), "chatgpt_web")
		}
		oauthPath = normalizeOAuthPathSetting(oauthPath)
		out["registration"] = map[string]any{
			"total":         asInt(m["total"], 1),
			"threads":       asInt(m["threads"], 1),
			"mode":          orStr(asString(m["mode"]), "protocol"),
			"pipeline_gate": orStr(asString(m["pipeline_gate"]), "reg"),
			"oauth_path":    oauthPath,
		}
	}
	if m, ok := ov["workspace"].(map[string]any); ok {
		ids := []any{}
		if raw, ok := m["ids"].([]any); ok {
			ids = raw
		}
		sel := asString(m["selected_id"])
		if sel == "" {
			sel = asString(m["id"])
		}
		if sel == "" && len(ids) > 0 {
			if s, ok := ids[0].(string); ok {
				sel = s
			} else {
				sel = strings.TrimSpace(fmt.Sprint(ids[0]))
			}
		}
		managers := normalizeManagers(m["managers"])
		// Backfill managers from legacy single session if empty
		if len(managers) == 0 {
			sf := orStr(asString(m["manager_session_file"]), "session.json")
			if sf != "" || sel != "" {
				managers = []any{map[string]any{
					"enabled":         true,
					"session_file":    sf,
					"quota":           20,
					"mailboxes_file":  "",
					"workspace_id":    sel,
					"email":           "",
					"domain":          "",
					"label":           "",
				}}
			}
		}
		if len(managers) > 0 {
			if mm, ok := managers[0].(map[string]any); ok {
				if sel == "" {
					sel = asString(mm["workspace_id"])
				}
			}
		}
		mailBinding := orStr(asString(m["mail_binding"]), "shared")
		out["workspace"] = map[string]any{
			"enabled":              asBool(m["enabled"], true),
			"ids":                  ids,
			"selected_id":          sel,
			"manager_session_file": orStr(asString(m["manager_session_file"]), "session.json"),
			"approve_requests":     asBool(m["approve_requests"], true),
			"mail_binding":         mailBinding,
			"managers":             managers,
		}
	}
	if m, ok := ov["proxy"].(map[string]any); ok {
		out["proxy"] = map[string]any{
			"proxies_file":     asString(m["proxies_file"]),
			"default_protocol": orStr(asString(m["default_protocol"]), "socks5"),
			"flaresolverr_url": asString(m["flaresolverr_url"]),
		}
	}
	return out
}

func loadOverlay(dataDir string) map[string]any {
	p := filepath.Join(dataDir, settingsFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func loadOverlayText(dataDir string) string {
	p := filepath.Join(dataDir, settingsFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return "{}\n"
	}
	return string(b)
}

func saveOverlayText(dataDir, text string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	if m == nil {
		return fmt.Errorf("根节点必须是 JSON 对象")
	}
	for k, v := range m {
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("节点 [%s] 必须是对象", k)
		}
	}
	return writeOverlay(dataDir, m)
}

func saveOverlayForm(dataDir string, incoming map[string]any) (map[string]any, error) {
	clean := sanitizeForm(incoming)
	cur := loadOverlay(dataDir)
	deepMerge(cur, clean)
	// flatten mail.mailboxes_file + alias_count + wait_* into providers[0]
	if mail, ok := clean["mail"].(map[string]any); ok {
		m := cur["mail"]
		if m == nil {
			cur["mail"] = map[string]any{}
			m = cur["mail"]
		}
		mm := m.(map[string]any)
		if mf, ok := mail["mailboxes_file"].(string); ok {
			mm["mailboxes_file"] = mf
		}
		if ac, ok := asIntVal(mail["alias_count"]); ok {
			if ac < 1 {
				ac = 1
			}
			if ac > 50 {
				ac = 50
			}
			mm["alias_count"] = ac
		}
		if wt, ok := asFloatVal(mail["wait_timeout"]); ok {
			mm["wait_timeout"] = wt
		}
		if wi, ok := asFloatVal(mail["wait_interval"]); ok {
			mm["wait_interval"] = wi
		}
		providers, _ := mm["providers"].([]any)
		if len(providers) == 0 {
			providers = []any{map[string]any{"type": "outlook_token", "enable": true, "mode": "graph"}}
		}
		if p0, ok := providers[0].(map[string]any); ok {
			if mf, ok := mm["mailboxes_file"].(string); ok && mf != "" {
				p0["mailboxes_file"] = mf
			}
			if ac, ok := asIntVal(mm["alias_count"]); ok && ac > 0 {
				p0["alias_count"] = ac
			}
			providers[0] = p0
		}
		mm["providers"] = providers
		cur["mail"] = mm
	}
	if err := writeOverlay(dataDir, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

func sanitizeForm(in map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := in["import_api"].(map[string]any); ok {
		out["import_api"] = sanitizeImportAPI(m)
	}
	if m, ok := in["codex_agent"].(map[string]any); ok {
		outDir := strings.TrimSpace(asString(m["output_dir"]))
		if outDir == "" {
			outDir = "codex_auth"
		}
		// Prevent path traversal in output_dir (relative name only).
		outDir = filepath.Base(outDir)
		if outDir == "." || outDir == ".." || outDir == "/" {
			outDir = "codex_auth"
		}
		verify := true
		if v, ok := m["verify_task"].(bool); ok {
			verify = v
		}
		out["codex_agent"] = map[string]any{
			"enabled":     m["enabled"] == true,
			"verify_task": verify,
			"output_dir":  outDir,
		}
	}
	if m, ok := in["registration"].(map[string]any); ok {
		oauthPath := orStr(asString(m["oauth_path"]), "")
		if oauthPath == "" {
			oauthPath = orStr(asString(m["auth_path"]), "chatgpt_web")
		}
		out["registration"] = map[string]any{
			"total":         asInt(m["total"], 1),
			"threads":       asInt(m["threads"], 1),
			"mode":          orStr(asString(m["mode"]), "protocol"),
			"pipeline_gate": orStr(asString(m["pipeline_gate"]), "reg"),
			"oauth_path":    normalizeOAuthPathSetting(oauthPath),
		}
	}
	if m, ok := in["workspace"].(map[string]any); ok {
		ids := []any{}
		switch t := m["ids"].(type) {
		case []any:
			for _, x := range t {
				if s := strings.TrimSpace(fmt.Sprint(x)); s != "" && s != "<nil>" {
					ids = append(ids, s)
				}
			}
		}
		sel := strings.TrimSpace(asString(m["selected_id"]))
		if sel == "" {
			sel = strings.TrimSpace(asString(m["id"]))
		}
		managers := normalizeManagers(m["managers"])
		// Sync legacy fields from first enabled manager for older runners / displays.
		mgrFile := orStr(asString(m["manager_session_file"]), "session.json")
		if len(managers) > 0 {
			if mm, ok := managers[0].(map[string]any); ok {
				if sf := strings.TrimSpace(asString(mm["session_file"])); sf != "" {
					mgrFile = sf
				}
				if wid := strings.TrimSpace(asString(mm["workspace_id"])); wid != "" {
					sel = wid
				}
			}
			// ids = all workspace ids from managers
			ids = []any{}
			for _, item := range managers {
				mm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if wid := strings.TrimSpace(asString(mm["workspace_id"])); wid != "" {
					ids = append(ids, wid)
				}
			}
		}
		// Ensure selected is in the pool; default to first.
		if sel != "" {
			found := false
			for _, x := range ids {
				if strings.EqualFold(fmt.Sprint(x), sel) {
					found = true
					break
				}
			}
			if !found {
				ids = append([]any{sel}, ids...)
			}
		} else if len(ids) > 0 {
			sel = fmt.Sprint(ids[0])
		}
		mailBinding := strings.ToLower(orStr(asString(m["mail_binding"]), "shared"))
		if mailBinding != "per_manager" && mailBinding != "shared" {
			mailBinding = "shared"
		}
		out["workspace"] = map[string]any{
			"enabled":              asBool(m["enabled"], true),
			"ids":                  ids,
			"selected_id":          sel,
			"manager_session_file": mgrFile,
			"approve_requests":     asBool(m["approve_requests"], true),
			"mail_binding":         mailBinding,
			"managers":             managers,
		}
	}
	if m, ok := in["proxy"].(map[string]any); ok {
		out["proxy"] = map[string]any{
			"proxies_file":     asString(m["proxies_file"]),
			"default_protocol": orStr(asString(m["default_protocol"]), "socks5"),
			"flaresolverr_url": asString(m["flaresolverr_url"]),
		}
	}
	if m, ok := in["mail"].(map[string]any); ok {
		ac := asInt(m["alias_count"], 1)
		if ac < 1 {
			ac = 1
		}
		if ac > 50 {
			ac = 50
		}
		wt := 30.0
		if v, ok := asFloatVal(m["wait_timeout"]); ok && v > 0 {
			wt = v
		}
		if wt < 5 {
			wt = 5
		}
		if wt > 300 {
			wt = 300
		}
		wi := 1.5
		if v, ok := asFloatVal(m["wait_interval"]); ok && v > 0 {
			wi = v
		}
		if wi < 0.3 {
			wi = 0.3
		}
		if wi > 30 {
			wi = 30
		}
		out["mail"] = map[string]any{
			"mailboxes_file": asString(m["mailboxes_file"]),
			"alias_count":    ac,
			"wait_timeout":   wt,
			"wait_interval":  wi,
		}
	}
	return out
}

// normalizeManagers cleans workspace.managers[] entries.
func normalizeManagers(raw any) []any {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return []any{}
	}
	out := []any{}
	for _, item := range list {
		em, ok := item.(map[string]any)
		if !ok {
			continue
		}
		sf := strings.TrimSpace(asString(em["session_file"]))
		if sf == "" {
			sf = strings.TrimSpace(asString(em["manager_session_file"]))
		}
		quota := asInt(em["quota"], 20)
		if quota < 1 {
			quota = 1
		}
		if quota > 10000 {
			quota = 10000
		}
		entry := map[string]any{
			"enabled":        asBool(em["enabled"], true),
			"session_file":   sf,
			"quota":          quota,
			"mailboxes_file": strings.TrimSpace(asString(em["mailboxes_file"])),
			"workspace_id":   strings.TrimSpace(asString(em["workspace_id"])),
			"email":          strings.TrimSpace(asString(em["email"])),
			"domain":         strings.TrimSpace(asString(em["domain"])),
			"label":          strings.TrimSpace(asString(em["label"])),
		}
		if entry["session_file"] == "" && entry["workspace_id"] == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func normalizeOAuthPathSetting(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "platform", "platform_oauth", "app_2sk", "legacy", "old":
		return "platform"
	default:
		return "chatgpt_web"
	}
}

// normalizeImportAPI converts legacy single-url shape into endpoints[].
func normalizeImportAPI(m map[string]any) map[string]any {
	reqK12 := asBool(m["require_k12"], true)
	eps := []any{}
	if raw, ok := m["endpoints"].([]any); ok && len(raw) > 0 {
		for i, item := range raw {
			em, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := asString(em["name"])
			if name == "" {
				name = fmt.Sprintf("api-%d", i+1)
			}
			eps = append(eps, map[string]any{
				"name":        name,
				"enabled":     asBool(em["enabled"], true),
				"url":         asString(em["url"]),
				"admin_key":   asString(em["admin_key"]),
				"require_k12": asBool(em["require_k12"], reqK12),
				"mode":        normalizeImportMode(asString(em["mode"])),
				"proxy_url":   asString(em["proxy_url"]),
			})
		}
	} else if url := asString(m["url"]); url != "" {
		// Legacy single import_api
		eps = append(eps, map[string]any{
			"name":        "default",
			"enabled":     asBool(m["enabled"], true),
			"url":         url,
			"admin_key":   asString(m["admin_key"]),
			"require_k12": reqK12,
			"mode":        normalizeImportMode(asString(m["mode"])),
			"proxy_url":   asString(m["proxy_url"]),
		})
	}
	return map[string]any{
		"require_k12": reqK12,
		"endpoints":   eps,
	}
}

func normalizeImportMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "agent_identity", "agent", "agent-identity", "agentidentity", "codex_agent", "codex-agent":
		return "agent_identity"
	default:
		return "at"
	}
}

func sanitizeImportAPI(m map[string]any) map[string]any {
	reqK12 := asBool(m["require_k12"], true)
	eps := []any{}
	if raw, ok := m["endpoints"].([]any); ok {
		for i, item := range raw {
			em, ok := item.(map[string]any)
			if !ok {
				continue
			}
			url := asString(em["url"])
			if url == "" {
				continue
			}
			name := asString(em["name"])
			if name == "" {
				name = fmt.Sprintf("api-%d", i+1)
			}
			eps = append(eps, map[string]any{
				"name":        name,
				"enabled":     asBool(em["enabled"], true),
				"url":         url,
				"admin_key":   asString(em["admin_key"]),
				"require_k12": asBool(em["require_k12"], reqK12),
				"mode":        normalizeImportMode(asString(em["mode"])),
				"proxy_url":   strings.TrimSpace(asString(em["proxy_url"])),
			})
		}
	}
	// Accept legacy fields on save as well
	if len(eps) == 0 {
		if url := asString(m["url"]); url != "" {
			eps = append(eps, map[string]any{
				"name":        "default",
				"enabled":     asBool(m["enabled"], true),
				"url":         url,
				"admin_key":   asString(m["admin_key"]),
				"require_k12": reqK12,
				"mode":        normalizeImportMode(asString(m["mode"])),
				"proxy_url":   strings.TrimSpace(asString(m["proxy_url"])),
			})
		}
	}
	// Persist only multi shape (+ keep first as legacy fields for older tools)
	out := map[string]any{
		"require_k12": reqK12,
		"endpoints":   eps,
	}
	if len(eps) > 0 {
		if e0, ok := eps[0].(map[string]any); ok {
			out["enabled"] = asBool(e0["enabled"], true)
			out["url"] = asString(e0["url"])
			out["admin_key"] = asString(e0["admin_key"])
		}
	} else {
		out["enabled"] = false
		out["url"] = ""
		out["admin_key"] = ""
	}
	return out
}

func writeOverlay(dataDir string, m map[string]any) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(dataDir, settingsFile), b, 0o644)
}

// persistWorkspaceSelected updates workspace.selected_id (and ensures it is in ids).
func persistWorkspaceSelected(dataDir, selectedID string) error {
	selectedID = strings.TrimSpace(selectedID)
	if selectedID == "" {
		return nil
	}
	cur := loadOverlay(dataDir)
	ws, _ := cur["workspace"].(map[string]any)
	if ws == nil {
		ws = map[string]any{}
	}
	ids := []any{}
	if raw, ok := ws["ids"].([]any); ok {
		ids = raw
	}
	found := false
	for _, x := range ids {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(x)), selectedID) {
			found = true
			break
		}
	}
	if !found {
		ids = append([]any{selectedID}, ids...)
	}
	ws["ids"] = ids
	ws["selected_id"] = selectedID
	cur["workspace"] = ws
	return writeOverlay(dataDir, cur)
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if vm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, vm)
				continue
			}
			// copy map
			cp := map[string]any{}
			deepMerge(cp, vm)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func asBool(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	default:
		return def
	}
}

func asInt(v any, def int) int {
	if n, ok := asIntVal(v); ok {
		return n
	}
	return def
}

func asIntVal(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		i, err := t.Int64()
		if err == nil {
			return int(i), true
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var n int
		_, err := fmt.Sscanf(s, "%d", &n)
		return n, err == nil
	}
	return 0, false
}

func asFloatVal(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var f float64
		_, err := fmt.Sscanf(s, "%f", &f)
		return f, err == nil
	}
	return 0, false
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
