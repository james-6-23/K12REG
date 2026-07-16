package workspace

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"k12reg/internal/httpx"
)

const ChatGPTBase = "https://chatgpt.com"

type JoinResult struct {
	OK          bool   `json:"ok"`
	StatusCode  int    `json:"status_code"`
	WorkspaceID string `json:"workspace_id"`
	Body        string `json:"body,omitempty"`
	Error       string `json:"error,omitempty"`
}

func Join(accessToken, workspaceID, route, proxy string) JoinResult {
	if route == "" {
		route = "request"
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return JoinResult{WorkspaceID: workspaceID, Error: err.Error()}
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)

	deviceID := randomID()
	u := fmt.Sprintf("%s/backend-api/accounts/%s/invites/%s", ChatGPTBase, workspaceID, route)
	headers := map[string]string{
		"accept":          "*/*",
		"authorization":   "Bearer " + accessToken,
		"content-type":    "application/json",
		"oai-device-id":    deviceID,
		"oai-language":    "en-US",
		"origin":          ChatGPTBase,
		"referer":         ChatGPTBase + "/",
		"user-agent":      httpx.UserAgent,
	}
	var last JoinResult
	// 3 tries, short backoff (1s/2s) — avoid multi-minute freezes on bad proxy.
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Post(u, []byte(""), headers, false)
		if err != nil {
			last = JoinResult{WorkspaceID: workspaceID, Error: err.Error()}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}
		body := httpx.DumpSnippet(resp.Body, 400)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return JoinResult{OK: true, StatusCode: resp.StatusCode, WorkspaceID: workspaceID, Body: body}
		}
		errMsg := strings.TrimSpace(body)
		if errMsg == "" {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		// Keep short error for logs; full body still in Body.
		if len(errMsg) > 180 {
			errMsg = errMsg[:180] + "…"
		}
		last = JoinResult{OK: false, StatusCode: resp.StatusCode, WorkspaceID: workspaceID, Body: body, Error: errMsg}
		// Auth-ish errors: no point retrying same token.
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return last
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	return last
}

type ManagerSession struct {
	Token     string
	AccountID string
	Email     string
}

func LoadManagerSession(path string) (ManagerSession, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ManagerSession{}, err
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return ManagerSession{}, err
	}
	token, _ := data["accessToken"].(string)
	if token == "" {
		token, _ = data["access_token"].(string)
	}
	accountID := ""
	if acc, ok := data["account"].(map[string]any); ok {
		accountID, _ = acc["id"].(string)
	}
	if accountID == "" {
		accountID, _ = data["account_id"].(string)
	}
	if accountID == "" {
		accountID, _ = data["chatgpt_account_id"].(string)
	}
	email := ""
	if user, ok := data["user"].(map[string]any); ok {
		email, _ = user["email"].(string)
	}
	if email == "" {
		email, _ = data["email"].(string)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ManagerSession{}, fmt.Errorf("no accessToken in %s", path)
	}
	return ManagerSession{
		Token:     token,
		AccountID: strings.TrimSpace(accountID),
		Email:     strings.TrimSpace(email),
	}, nil
}

// RefreshAccessToken exchanges a platform refresh_token for a new access_token.
func RefreshAccessToken(refreshToken, proxy string) (accessToken string, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", fmt.Errorf("empty refresh_token")
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return "", err
	}
	defer client.Close()
	form := url.Values{}
	form.Set("client_id", "app_2SKx67EdpoN0G6j64rFvigXD")
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	resp, err := client.PostForm("https://auth.openai.com/oauth/token", form, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("oauth refresh HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160))
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	at, _ := data["access_token"].(string)
	if strings.TrimSpace(at) == "" {
		return "", fmt.Errorf("oauth refresh missing access_token")
	}
	return at, nil
}

// ApproveByEmail lists pending requests and accepts the matching invite.
func ApproveByEmail(mgr ManagerSession, email, proxy string, maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 8
	}
	if maxAttempts > 12 {
		maxAttempts = 12
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return err
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)
	email = strings.ToLower(strings.TrimSpace(email))
	accountID := mgr.AccountID
	if accountID == "" {
		return fmt.Errorf("manager session missing account id")
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		items, err := listRequests(client, mgr, email)
		if err == nil {
			for _, it := range items {
				got := strings.ToLower(strings.TrimSpace(str(it["email_address"])))
				if got == "" {
					got = strings.ToLower(strings.TrimSpace(str(it["email"])))
				}
				if got != email && !strings.Contains(got, email) && !strings.Contains(email, got) {
					// plus-alias loose match
					if !aliasMatch(email, got) {
						continue
					}
				}
				id := str(it["id"])
				if id == "" {
					continue
				}
				if err := acceptRequest(client, mgr, id); err != nil {
					return err
				}
				return nil
			}
		}
		// Cap wait: 1.5s → 3s (was 2+attempt up to 13s each → ~90s pure sleep).
		if attempt+1 < maxAttempts {
			time.Sleep(approveBackoff(attempt))
		}
	}
	return fmt.Errorf("approve timeout for %s", email)
}

// approveBackoff: short, capped delays between invite list polls.
func approveBackoff(attempt int) time.Duration {
	// 1.5s, 2s, 2.5s, 3s, 3s, …
	d := 1500*time.Millisecond + time.Duration(attempt)*500*time.Millisecond
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	return d
}

func aliasMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	al, _, _ := strings.Cut(a, "@")
	bl, _, _ := strings.Cut(b, "@")
	al = strings.Split(al, "+")[0]
	bl = strings.Split(bl, "+")[0]
	return al != "" && al == bl
}

func listRequests(client *httpx.Client, mgr ManagerSession, query string) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("include_pending", "false")
	q.Set("include_requests", "true")
	q.Set("offset", "0")
	q.Set("limit", "25")
	q.Set("query", query)
	u := fmt.Sprintf("%s/backend-api/accounts/%s/invites?%s", ChatGPTBase, mgr.AccountID, q.Encode())
	headers := managerHeaders(mgr, fmt.Sprintf("/backend-api/accounts/%s/invites", mgr.AccountID))
	resp, err := client.Get(u, headers, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list invites HTTP %d", resp.StatusCode)
	}
	var data struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(resp.Body, &data)
	return data.Items, nil
}

func acceptRequest(client *httpx.Client, mgr ManagerSession, inviteID string) error {
	u := fmt.Sprintf("%s/backend-api/accounts/%s/invites/%s", ChatGPTBase, mgr.AccountID, inviteID)
	headers := managerHeaders(mgr, fmt.Sprintf("/backend-api/accounts/%s/invites/%s", mgr.AccountID, inviteID))
	payload, _ := json.Marshal(map[string]any{"accept_request": true})
	resp, err := client.Do("PATCH", u, payload, headers, false)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("accept invite HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
	}
	return nil
}

func managerHeaders(mgr ManagerSession, targetPath string) map[string]string {
	return map[string]string{
		"accept":               "*/*",
		"authorization":        "Bearer " + mgr.Token,
		"chatgpt-account-id":   mgr.AccountID,
		"content-type":         "application/json",
		"oai-device-id":         randomID(),
		"oai-language":         "en-US",
		"x-openai-target-path": targetPath,
		"referer":              "https://chatgpt.com/admin/members?tab=requests",
		"user-agent":           httpx.UserAgent,
	}
}

// CheckPlan calls accounts/check and returns plan_type + account_id if present.
// preferred workspace ids are tried first; also retries with chatgpt-account-id header.
func CheckPlan(accessToken, proxy string, preferred []string) (plan, accountID string, err error) {
	client, err := httpx.New(proxy)
	if err != nil {
		return "", "", err
	}
	defer client.Close()

	try := func(extra map[string]string) (string, string, error) {
		u := ChatGPTBase + "/backend-api/accounts/check/v4-2023-04-27"
		headers := map[string]string{
			"authorization": "Bearer " + accessToken,
			"accept":        "*/*",
			"oai-language":  "en-US",
			"user-agent":    httpx.UserAgent,
		}
		for k, v := range extra {
			headers[k] = v
		}
		resp, err := client.Get(u, headers, false)
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode != 200 {
			return "", "", fmt.Errorf("accounts/check HTTP %d", resp.StatusCode)
		}
		var data map[string]any
		_ = json.Unmarshal(resp.Body, &data)
		return pickPlan(data, preferred)
	}

	plan, accountID, err = try(nil)
	if err != nil {
		return "", "", err
	}
	// If still free but we have preferred workspace, re-check with that account header.
	if !isTeamish(plan) && len(preferred) > 0 {
		if p2, a2, e2 := try(map[string]string{"chatgpt-account-id": preferred[0]}); e2 == nil {
			if isTeamish(p2) || a2 != "" {
				return p2, a2, nil
			}
		}
	}
	return plan, accountID, nil
}

func isTeamish(plan string) bool {
	switch strings.ToLower(plan) {
	case "k12", "team", "enterprise", "edu", "business":
		return true
	default:
		return false
	}
}

func pickPlan(data map[string]any, preferred []string) (plan, accountID string, err error) {
	accts, _ := data["accounts"].(map[string]any)
	pref := map[string]bool{}
	for _, p := range preferred {
		pref[strings.ToLower(strings.TrimSpace(p))] = true
	}
	type ent struct{ plan, id, key string }
	var entries []ent
	for k, v := range accts {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		acc, _ := m["account"].(map[string]any)
		p := str(acc["plan_type"])
		id := str(acc["account_id"])
		if id == "" {
			id = k
		}
		entries = append(entries, ent{plan: p, id: id, key: k})
	}
	for _, e := range entries {
		if pref[strings.ToLower(e.id)] {
			return e.plan, e.id, nil
		}
	}
	for _, e := range entries {
		if isTeamish(e.plan) {
			return e.plan, e.id, nil
		}
	}
	if len(entries) > 0 {
		return entries[0].plan, entries[0].id, nil
	}
	return "", "", nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func randomID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
