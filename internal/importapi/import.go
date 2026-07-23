// Package importapi pushes accounts into codex2api (or compatible) admin pools.
//
// Modes:
//   - access_token: POST /api/admin/accounts/at  {access_token}
//   - agent_identity: POST /api/admin/accounts/codex/agent-identity
//     and batch POST .../agent-identity/import  (see AGENT_IDENTITY_IMPORT.md)
package importapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"k12reg/internal/httpx"
)

const (
	// EndpointAT is the legacy access_token import path.
	EndpointAT = "/api/admin/accounts/at"
	// EndpointAgentIdentity single auth.json import.
	EndpointAgentIdentity = "/api/admin/accounts/codex/agent-identity"
	// EndpointAgentIdentityImport batch auth.json import (max 200 per request).
	EndpointAgentIdentityImport = "/api/admin/accounts/codex/agent-identity/import"

	// ModeAT pushes access_token (default / legacy).
	ModeAT = "at"
	// ModeAgentIdentity pushes Codex Agent Identity auth.json.
	ModeAgentIdentity = "agent_identity"

	// MaxAgentIdentityBatch is the gateway hard limit per import request.
	MaxAgentIdentityBatch = 200
)

// Result of one import POST.
type Result struct {
	OK      bool
	Status  int
	Outcome string // added | updated | duplicate | failed | unknown
	Message string
	Error   string
	// Optional fields from agent-identity responses
	ID    any    `json:"-"`
	Email string `json:"-"`
}

// BatchItem is one entry from batch agent-identity import.
type BatchItem struct {
	Email string `json:"email,omitempty"`
	ID    any    `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// BatchResult is the gateway response for agent-identity/import.
type BatchResult struct {
	Total    int
	Imported int
	Failed   int
	Items    []BatchItem
	OK       bool
	Status   int
	Error    string
	Raw      map[string]any
}

// NormalizeMode maps UI / settings aliases to ModeAT | ModeAgentIdentity.
func NormalizeMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ModeAT, "access_token", "token", "oauth", "jwt":
		return ModeAT
	case ModeAgentIdentity, "agent", "agent-identity", "agentidentity", "codex_agent", "codex-agent":
		return ModeAgentIdentity
	default:
		return ModeAT
	}
}

// Push posts access_token to {baseURL}/api/admin/accounts/at.
func Push(baseURL, adminKey, accessToken, proxy string) Result {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || accessToken == "" {
		return Result{OK: false, Outcome: "failed", Error: "missing url or token"}
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	defer client.Close()

	u := baseURL + EndpointAT
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	resp, err := client.PostJSON(u, body, map[string]string{
		"X-Admin-Key": adminKey,
		"Accept":      "application/json",
	}, false)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{
			OK: false, Status: resp.StatusCode, Outcome: "failed",
			Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160)),
		}
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	msg, _ := data["message"].(string)
	outcome := classify(data)
	return Result{OK: outcome != "failed", Status: resp.StatusCode, Outcome: outcome, Message: msg}
}

// PushAgentIdentity imports one auth.json via entry A (single).
// authJSON is the raw auth.json object (map or already-marshaled string content).
// Optional name is display name; proxyURL is stored on the gateway account (not our egress proxy).
// httpProxy is optional local egress for the admin request itself.
func PushAgentIdentity(baseURL, adminKey string, authJSON any, name, accountProxyURL, httpProxy string) Result {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return Result{OK: false, Outcome: "failed", Error: "missing url"}
	}
	authStr, err := authJSONToString(authJSON)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	if authStr == "" {
		return Result{OK: false, Outcome: "failed", Error: "empty auth_json"}
	}

	client, err := httpx.New(httpProxy)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	defer client.Close()

	payload := map[string]any{"auth_json": authStr}
	if n := strings.TrimSpace(name); n != "" {
		payload["name"] = n
	}
	if p := strings.TrimSpace(accountProxyURL); p != "" {
		payload["proxy_url"] = p
	}
	body, _ := json.Marshal(payload)
	u := baseURL + EndpointAgentIdentity
	resp, err := client.PostJSON(u, body, map[string]string{
		"X-Admin-Key": adminKey,
		"Accept":      "application/json",
	}, false)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}

	// 409 = already exists (dedupe by agent_runtime_id) — treat as soft success.
	if resp.StatusCode == 409 {
		var data map[string]any
		_ = json.Unmarshal(resp.Body, &data)
		msg, _ := data["error"].(string)
		if msg == "" {
			msg, _ = data["message"].(string)
		}
		if msg == "" {
			msg = "该 Agent Identity 账号已存在"
		}
		return Result{
			OK: true, Status: 409, Outcome: "duplicate", Message: msg,
			Error: msg, Email: strAny(data["email"]),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{
			OK: false, Status: resp.StatusCode, Outcome: "failed",
			Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200)),
		}
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	msg, _ := data["message"].(string)
	email := strAny(data["email"])
	return Result{
		OK: true, Status: resp.StatusCode, Outcome: "added", Message: msg,
		ID: data["id"], Email: email,
	}
}

// PushAgentIdentityBatch imports many auth.json strings (entry B, max 200).
func PushAgentIdentityBatch(baseURL, adminKey string, files []string, accountProxyURL, httpProxy string) BatchResult {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return BatchResult{OK: false, Error: "missing url"}
	}
	if len(files) == 0 {
		return BatchResult{OK: false, Error: "empty files"}
	}
	if len(files) > MaxAgentIdentityBatch {
		return BatchResult{OK: false, Error: fmt.Sprintf("batch size %d exceeds max %d", len(files), MaxAgentIdentityBatch)}
	}

	client, err := httpx.New(httpProxy)
	if err != nil {
		return BatchResult{OK: false, Error: err.Error()}
	}
	defer client.Close()

	payload := map[string]any{"files": files}
	if p := strings.TrimSpace(accountProxyURL); p != "" {
		payload["proxy_url"] = p
	}
	body, _ := json.Marshal(payload)
	u := baseURL + EndpointAgentIdentityImport
	resp, err := client.PostJSON(u, body, map[string]string{
		"X-Admin-Key": adminKey,
		"Accept":      "application/json",
	}, false)
	if err != nil {
		return BatchResult{OK: false, Error: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BatchResult{
			OK: false, Status: resp.StatusCode,
			Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 240)),
		}
	}
	var data map[string]any
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return BatchResult{OK: false, Status: resp.StatusCode, Error: "invalid json response"}
	}
	br := BatchResult{
		OK:       true,
		Status:   resp.StatusCode,
		Total:    asInt(data["total"]),
		Imported: asInt(data["imported"]),
		Failed:   asInt(data["failed"]),
		Raw:      data,
	}
	if raw, ok := data["items"].([]any); ok {
		for _, it := range raw {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			br.Items = append(br.Items, BatchItem{
				Email: strAny(m["email"]),
				ID:    m["id"],
				OK:    m["ok"] == true,
				Error: strAny(m["error"]),
			})
		}
	}
	return br
}

// PushAgentIdentityBatchAll chunks files into MaxAgentIdentityBatch-sized requests.
func PushAgentIdentityBatchAll(baseURL, adminKey string, files []string, accountProxyURL, httpProxy string) BatchResult {
	if len(files) == 0 {
		return BatchResult{OK: false, Error: "empty files"}
	}
	if len(files) <= MaxAgentIdentityBatch {
		return PushAgentIdentityBatch(baseURL, adminKey, files, accountProxyURL, httpProxy)
	}
	agg := BatchResult{OK: true}
	for i := 0; i < len(files); i += MaxAgentIdentityBatch {
		end := i + MaxAgentIdentityBatch
		if end > len(files) {
			end = len(files)
		}
		part := PushAgentIdentityBatch(baseURL, adminKey, files[i:end], accountProxyURL, httpProxy)
		if !part.OK {
			agg.OK = false
			if agg.Error == "" {
				agg.Error = part.Error
			}
			agg.Failed += len(files[i:end])
			agg.Total += len(files[i:end])
			continue
		}
		agg.Total += part.Total
		agg.Imported += part.Imported
		agg.Failed += part.Failed
		agg.Items = append(agg.Items, part.Items...)
		agg.Status = part.Status
	}
	return agg
}

func authJSONToString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return "", fmt.Errorf("empty auth_json string")
		}
		// If already a JSON object string, pass through; if raw object without braces, reject.
		return s, nil
	case []byte:
		return strings.TrimSpace(string(t)), nil
	case nil:
		return "", fmt.Errorf("nil auth_json")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func classify(data map[string]any) string {
	if data == nil {
		return "unknown"
	}
	n := func(k string) int {
		return asInt(data[k])
	}
	switch {
	case n("success") > 0 || n("added") > 0:
		return "added"
	case n("updated") > 0:
		return "updated"
	case n("duplicate") > 0:
		return "duplicate"
	case n("failed") > 0:
		return "failed"
	default:
		return "unknown"
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return 0
	}
}

func strAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
