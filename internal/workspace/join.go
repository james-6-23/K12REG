package workspace

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"k12reg/internal/httpx"
)

// One manager account: serialize approve so N workers don't stampede list/PATCH
// and miss each other's invites (main cause of "approve timeout" under threads>3).
var approveMuByAccount sync.Map // accountID → *sync.Mutex

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

// RefreshAccessToken exchanges a refresh_token for a new access_token.
// Tries ChatGPT Web client first (app_X8z…), then legacy Platform client.
// Prefers auth.openai.com/api/accounts/oauth/token JSON (same as signup exchange).
func RefreshAccessToken(refreshToken, proxy string) (accessToken string, err error) {
	at, _, err := RefreshTokens(refreshToken, proxy)
	return at, err
}

// RefreshTokens is like RefreshAccessToken but also returns a rotated refresh_token
// when OpenAI issues one (caller should persist it).
func RefreshTokens(refreshToken, proxy string) (accessToken, newRefresh string, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", fmt.Errorf("empty refresh_token")
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return "", "", err
	}
	defer client.Close()

	const webClient = "app_X8zY6vW2pQ9tR3dE7nK1jL5gH"
	const platformClient = "app_2SKx67EdpoN0G6j64rFvigXD"

	tryJSON := func(clientID string) (string, string, error) {
		payload, _ := json.Marshal(map[string]string{
			"client_id":     clientID,
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
		})
		headers := map[string]string{
			"accept":       "application/json",
			"content-type": "application/json",
			"origin":       "https://chatgpt.com",
			"referer":      "https://chatgpt.com/",
			"user-agent":   httpx.UserAgent,
		}
		resp, err := client.PostJSON("https://auth.openai.com/api/accounts/oauth/token", payload, headers, false)
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode != 200 {
			return "", "", fmt.Errorf("oauth refresh HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160))
		}
		var data map[string]any
		_ = json.Unmarshal(resp.Body, &data)
		at, _ := data["access_token"].(string)
		rt, _ := data["refresh_token"].(string)
		if strings.TrimSpace(at) == "" {
			return "", "", fmt.Errorf("oauth refresh missing access_token")
		}
		return strings.TrimSpace(at), strings.TrimSpace(rt), nil
	}

	tryForm := func(clientID string) (string, string, error) {
		form := url.Values{}
		form.Set("client_id", clientID)
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		resp, err := client.PostForm("https://auth.openai.com/oauth/token", form, map[string]string{
			"Accept": "application/json",
		})
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode != 200 {
			return "", "", fmt.Errorf("oauth refresh form HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160))
		}
		var data map[string]any
		_ = json.Unmarshal(resp.Body, &data)
		at, _ := data["access_token"].(string)
		rt, _ := data["refresh_token"].(string)
		if strings.TrimSpace(at) == "" {
			return "", "", fmt.Errorf("oauth refresh missing access_token")
		}
		return strings.TrimSpace(at), strings.TrimSpace(rt), nil
	}

	var last error
	for _, clientID := range []string{webClient, platformClient} {
		if at, rt, e := tryJSON(clientID); e == nil {
			return at, rt, nil
		} else {
			last = e
		}
		if at, rt, e := tryForm(clientID); e == nil {
			return at, rt, nil
		} else {
			last = e
		}
	}
	return "", "", last
}

func managerApproveLock(accountID string) *sync.Mutex {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		accountID = "_"
	}
	v, _ := approveMuByAccount.LoadOrStore(accountID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ApproveByEmail lists pending requests and accepts the matching invite.
// Concurrent workers sharing one manager are serialized per account id.
func ApproveByEmail(mgr ManagerSession, email, proxy string, maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 8
	}
	if maxAttempts > 20 {
		maxAttempts = 20
	}
	email = strings.ToLower(strings.TrimSpace(email))
	accountID := strings.TrimSpace(mgr.AccountID)
	if accountID == "" {
		return fmt.Errorf("manager session missing account id")
	}

	// Serialize per manager: concurrent list+accept races miss invites under load.
	mu := managerApproveLock(accountID)
	mu.Lock()
	defer mu.Unlock()

	client, err := httpx.New(proxy)
	if err != nil {
		return err
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)

	// Join → invite visibility often lags 1–3s; first poll too early burns an attempt.
	time.Sleep(1200 * time.Millisecond)

	var lastListErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		items, err := findInviteItems(client, mgr, email)
		if err != nil {
			lastListErr = err
		} else if id, got := matchInviteID(items, email); id != "" {
			if err := acceptRequest(client, mgr, id); err != nil {
				// Transient: invite may have been accepted by another path — re-list once.
				if attempt+1 < maxAttempts {
					time.Sleep(approveBackoff(attempt))
					continue
				}
				return err
			}
			_ = got
			return nil
		}
		if attempt+1 < maxAttempts {
			time.Sleep(approveBackoff(attempt))
		}
	}
	if lastListErr != nil {
		return fmt.Errorf("approve timeout for %s (last list: %v)", email, lastListErr)
	}
	return fmt.Errorf("approve timeout for %s", email)
}

// approveBackoff: slightly longer early waits so invites can appear under concurrency.
func approveBackoff(attempt int) time.Duration {
	// 1.2s, 1.8s, 2.4s, 3s, 3.5s, 4s cap
	d := 1200*time.Millisecond + time.Duration(attempt)*600*time.Millisecond
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// emailLocalBase strips plus-tag: "a+xyz@outlook.com" → "a".
func emailLocalBase(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	local, _, ok := strings.Cut(email, "@")
	if !ok {
		return email
	}
	return strings.Split(local, "+")[0]
}

func emailDomainPart(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	_, d, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return d
}

// inviteEmail extracts email fields from an invite item.
func inviteEmail(it map[string]any) string {
	for _, k := range []string{"email_address", "email", "invitee_email", "user_email"} {
		if s := strings.ToLower(strings.TrimSpace(str(it[k]))); s != "" && strings.Contains(s, "@") {
			return s
		}
	}
	// Nested shapes.
	if u, ok := it["user"].(map[string]any); ok {
		if s := strings.ToLower(strings.TrimSpace(str(u["email"]))); s != "" {
			return s
		}
	}
	return ""
}

// emailMatch: exact, plus-alias base, or contains — case-insensitive.
func emailMatch(want, got string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	got = strings.ToLower(strings.TrimSpace(got))
	if want == "" || got == "" {
		return false
	}
	if want == got {
		return true
	}
	if strings.Contains(got, want) || strings.Contains(want, got) {
		return true
	}
	return aliasMatch(want, got)
}

func aliasMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	al := emailLocalBase(a)
	bl := emailLocalBase(b)
	if al == "" || al != bl {
		return false
	}
	// Same mailbox base; domain should match when both present.
	da, db := emailDomainPart(a), emailDomainPart(b)
	if da != "" && db != "" && da != db {
		return false
	}
	return true
}

func matchInviteID(items []map[string]any, email string) (id, matchedEmail string) {
	for _, it := range items {
		got := inviteEmail(it)
		if !emailMatch(email, got) {
			continue
		}
		id = str(it["id"])
		if id == "" {
			continue
		}
		return id, got
	}
	return "", ""
}

// findInviteItems tries several list queries: full email, local@domain base,
// bare local base, then unfiltered recent page (concurrency-safe matching).
func findInviteItems(client *httpx.Client, mgr ManagerSession, email string) ([]map[string]any, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	base := emailLocalBase(email)
	dom := emailDomainPart(email)
	queries := []string{email}
	if base != "" && dom != "" {
		queries = append(queries, base+"@"+dom)
	}
	if base != "" {
		queries = append(queries, base)
	}
	queries = append(queries, "") // full recent list

	var all []map[string]any
	seen := map[string]bool{}
	var lastErr error
	for _, q := range queries {
		items, err := listRequests(client, mgr, q)
		if err != nil {
			lastErr = err
			continue
		}
		for _, it := range items {
			id := str(it["id"])
			key := id
			if key == "" {
				key = inviteEmail(it)
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, it)
		}
		// Fast path: already have a match.
		if id, _ := matchInviteID(items, email); id != "" {
			return all, nil
		}
	}
	if len(all) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return all, nil
}

func listRequests(client *httpx.Client, mgr ManagerSession, query string) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("include_pending", "true")
	q.Set("include_requests", "true")
	q.Set("offset", "0")
	q.Set("limit", "50")
	if strings.TrimSpace(query) != "" {
		q.Set("query", query)
	}
	u := fmt.Sprintf("%s/backend-api/accounts/%s/invites?%s", ChatGPTBase, mgr.AccountID, q.Encode())
	headers := managerHeaders(mgr, fmt.Sprintf("/backend-api/accounts/%s/invites", mgr.AccountID))
	resp, err := client.Get(u, headers, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list invites HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 120))
	}
	var data struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		// Some responses wrap differently.
		var alt map[string]any
		if json.Unmarshal(resp.Body, &alt) == nil {
			if raw, ok := alt["items"].([]any); ok {
				for _, r := range raw {
					if m, ok := r.(map[string]any); ok {
						data.Items = append(data.Items, m)
					}
				}
			}
		}
	}
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
