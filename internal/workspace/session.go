package workspace

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"k12reg/internal/httpx"
)

// WorkspacePlans are org/team seats we treat as successful elevate targets.
// Personal free/plus/pro do not count.
var WorkspacePlans = map[string]bool{
	"k12": true, "team": true, "enterprise": true, "edu": true, "business": true,
}

const sessionCookieName = "__Secure-next-auth.session-token"
const sessionCookieLegacy = "next-auth.session-token"

// JWTPlanType returns chatgpt_plan_type from an access_token JWT (lowercase),
// or empty when the claim is absent. Does not default to "free".
func JWTPlanType(accessToken string) string {
	auth := jwtAuthClaims(accessToken)
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(str(auth["chatgpt_plan_type"])))
}

// JWTAccountID returns chatgpt_account_id from the access_token JWT.
func JWTAccountID(accessToken string) string {
	auth := jwtAuthClaims(accessToken)
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(str(auth["chatgpt_account_id"]))
}

// JWTIsWorkspaceScoped is true when the AT JWT itself carries a workspace plan.
// Check-API labels alone are not enough — protocol tokens can be free while
// membership shows k12 on /accounts/check.
func JWTIsWorkspaceScoped(accessToken string) bool {
	return WorkspacePlans[JWTPlanType(accessToken)]
}

// IsWorkspacePlan reports whether a plan_type string is k12/team/…
func IsWorkspacePlan(plan string) bool {
	return WorkspacePlans[strings.ToLower(strings.TrimSpace(plan))]
}

func jwtAuthClaims(accessToken string) map[string]any {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some tokens use padded base64url.
		payload, err = base64.URLEncoding.DecodeString(padB64(parts[1]))
		if err != nil {
			return nil
		}
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil
	}
	auth, _ := data["https://api.openai.com/auth"].(map[string]any)
	return auth
}

func padB64(s string) string {
	switch len(s) % 4 {
	case 2:
		return s + "=="
	case 3:
		return s + "="
	default:
		return s
	}
}

// SessionFields is the flat account payload from /api/auth/session.
type SessionFields struct {
	AccessToken       string
	SessionToken      string
	PlanType          string
	ChatGPTAccountID  string
	Expires           string
	Email             string
}

// FetchSession calls GET chatgpt.com/api/auth/session with the NextAuth cookie.
// When accountID is non-empty, pins workspace via cookies + headers so the
// returned accessToken is workspace-scoped (k12/team) after join+approve.
func FetchSession(sessionToken, accountID, proxy string) (SessionFields, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return SessionFields{}, fmt.Errorf("empty session_token")
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return SessionFields{}, err
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)

	accountID = strings.TrimSpace(accountID)
	// Seed session cookie on chatgpt.com.
	client.SetCookie(sessionCookieName, sessionToken, ".chatgpt.com")
	client.SetCookie(sessionCookieName, sessionToken, "chatgpt.com")
	if accountID != "" {
		for _, name := range []string{"oai-account-id", "_account", "chatgpt-account-id"} {
			client.SetCookie(name, accountID, ".chatgpt.com")
			client.SetCookie(name, accountID, "chatgpt.com")
		}
	}

	u := ChatGPTBase + "/api/auth/session"
	if accountID != "" {
		u += "?account_id=" + url.QueryEscape(accountID)
	}
	headers := map[string]string{
		"accept":     "application/json",
		"referer":    ChatGPTBase + "/",
		"origin":     ChatGPTBase,
		"user-agent": httpx.UserAgent,
	}
	if accountID != "" {
		headers["chatgpt-account-id"] = accountID
		headers["Chatgpt-Account-Id"] = accountID
	}

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		resp, err := client.Get(u, headers, false)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
			continue
		}
		if resp.StatusCode != 200 {
			body := strings.ToLower(httpx.DumpSnippet(resp.Body, 80))
			// CF / empty — retry
			if resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500 ||
				strings.Contains(body, "<html") || strings.Contains(body, "<!doctype") {
				lastErr = fmt.Errorf("auth/session HTTP %d", resp.StatusCode)
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
				continue
			}
			return SessionFields{}, fmt.Errorf("auth/session HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160))
		}
		var data map[string]any
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
			continue
		}
		fields := sessionPayloadToFields(data)
		if fields.SessionToken == "" {
			fields.SessionToken = sessionToken
		}
		// Prefer cookie refresh if session rotated.
		if st := client.Cookie(ChatGPTBase+"/", sessionCookieName); st != "" {
			fields.SessionToken = st
		} else if st := client.Cookie(ChatGPTBase+"/", sessionCookieLegacy); st != "" {
			fields.SessionToken = st
		}
		if fields.AccessToken == "" {
			lastErr = fmt.Errorf("auth/session missing accessToken")
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			continue
		}
		// Prefer JWT plan when JSON omits planType.
		if !IsWorkspacePlan(fields.PlanType) {
			if jp := JWTPlanType(fields.AccessToken); jp != "" {
				fields.PlanType = jp
			}
		}
		if fields.ChatGPTAccountID == "" {
			fields.ChatGPTAccountID = JWTAccountID(fields.AccessToken)
		}
		return fields, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("auth/session failed")
	}
	return SessionFields{}, lastErr
}

func sessionPayloadToFields(data map[string]any) SessionFields {
	f := SessionFields{}
	if v, ok := data["accessToken"].(string); ok {
		f.AccessToken = strings.TrimSpace(v)
	}
	if f.AccessToken == "" {
		if v, ok := data["access_token"].(string); ok {
			f.AccessToken = strings.TrimSpace(v)
		}
	}
	if v, ok := data["sessionToken"].(string); ok {
		f.SessionToken = strings.TrimSpace(v)
	}
	if f.SessionToken == "" {
		if v, ok := data["session_token"].(string); ok {
			f.SessionToken = strings.TrimSpace(v)
		}
	}
	if v, ok := data["expires"].(string); ok {
		f.Expires = strings.TrimSpace(v)
	}
	if user, ok := data["user"].(map[string]any); ok {
		f.Email = strings.TrimSpace(str(user["email"]))
	}
	if acc, ok := data["account"].(map[string]any); ok {
		f.ChatGPTAccountID = strings.TrimSpace(str(acc["id"]))
		f.PlanType = strings.ToLower(strings.TrimSpace(str(acc["planType"])))
		if f.PlanType == "" {
			f.PlanType = strings.ToLower(strings.TrimSpace(str(acc["plan_type"])))
		}
	}
	return f
}

// ElevateSession re-fetches /api/auth/session for each preferred workspace id
// until the returned access token is workspace-scoped (or account id matches).
func ElevateSession(sessionToken string, workspaceIDs []string, proxy string) (SessionFields, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return SessionFields{}, fmt.Errorf("empty session_token")
	}
	var lastErr error
	for _, wid := range workspaceIDs {
		wid = strings.TrimSpace(wid)
		if wid == "" {
			continue
		}
		fields, err := FetchSession(sessionToken, wid, proxy)
		if err != nil {
			lastErr = err
			continue
		}
		plan := strings.ToLower(fields.PlanType)
		aid := strings.TrimSpace(fields.ChatGPTAccountID)
		jwtPlan := JWTPlanType(fields.AccessToken)
		if jwtPlan != "" {
			plan = jwtPlan
			fields.PlanType = jwtPlan
		}
		if jwtAID := JWTAccountID(fields.AccessToken); jwtAID != "" {
			aid = jwtAID
			fields.ChatGPTAccountID = jwtAID
		}
		// Success: JWT/workspace plan, or account id pinned to requested workspace.
		if WorkspacePlans[plan] || (aid != "" && strings.EqualFold(aid, wid)) {
			if !WorkspacePlans[plan] && strings.EqualFold(aid, wid) {
				// Session may omit planType; membership pin implies k12 for our use.
				fields.PlanType = "k12"
			}
			if fields.ChatGPTAccountID == "" {
				fields.ChatGPTAccountID = wid
			}
			return fields, nil
		}
		lastErr = fmt.Errorf("session still plan=%s aid=%s for ws=%s", plan, truncID(aid), truncID(wid))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no workspace ids to elevate")
	}
	return SessionFields{}, lastErr
}
