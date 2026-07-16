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

// SessionWarning matches chatgpt.com /api/auth/session banner text.
const SessionWarning = "!!!!!!!!!!!!!!!!!!!! DO NOT SHARE ANY PART OF THE INFORMATION YOU SEE HERE. THIS INFORMATION IS SENSITIVE AND CAN GRANT ACCESS TO YOUR ACCOUNT. SHARING THIS INFORMATION IS LIKE SHARING YOUR PASSWORD. !!!!!!!!!!!!!!!!!!!!"

// JoinOwnerRequest is the one-shot join + approve(+owner) + session_after flow.
type JoinOwnerRequest struct {
	ManagerSession any    // raw JSON object or string
	TargetSession  any    // raw JSON object or string
	Proxy          string // optional socks/http proxy URL
	SetOwner       bool   // default true when omitted by caller
	MaxApprove     int    // default 12
}

// JoinOwnerResult is returned to the web UI / API clients.
type JoinOwnerResult struct {
	OK           bool           `json:"ok"`
	Error        string         `json:"error,omitempty"`
	WorkspaceID  string         `json:"workspace_id,omitempty"`
	Manager      SessionSummary `json:"manager,omitempty"`
	Target       SessionSummary `json:"target,omitempty"`
	Join         JoinResult     `json:"join"`
	Approve      *ApproveResult `json:"approve,omitempty"`
	Owner        *OwnerResult   `json:"owner,omitempty"`
	Plan         *PlanInfo      `json:"plan,omitempty"`
	SessionAfter map[string]any `json:"session_after,omitempty"`
	AccountsCheck any           `json:"accounts_check,omitempty"`
	Logs         []LogLine      `json:"logs"`
	ProxyUsed    string         `json:"proxy_used,omitempty"`
}

type SessionSummary struct {
	Email     string `json:"email,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	Expires   string `json:"expires,omitempty"`
}

type ApproveResult struct {
	OK       bool   `json:"ok"`
	InviteID string `json:"invite_id,omitempty"`
	Email    string `json:"email,omitempty"`
	Error    string `json:"error,omitempty"`
}

type OwnerResult struct {
	OK       bool   `json:"ok"`
	Role     string `json:"role,omitempty"`
	InviteID string `json:"invite_id,omitempty"`
	Via      string `json:"via,omitempty"`
	Error    string `json:"error,omitempty"`
}

type PlanInfo struct {
	Plan      string `json:"plan,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Role      string `json:"role,omitempty"`
}

type LogLine struct {
	T     int64  `json:"t"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// ParsedSession is a normalized session blob.
type ParsedSession struct {
	AccessToken string
	Email       string
	UserID      string
	AccountID   string
	PlanType    string
	Expires     string
	Raw         map[string]any
}

// JoinAndSetOwner: target requests join → manager approves (optionally as owner) → accounts/check → session_after.
func JoinAndSetOwner(req JoinOwnerRequest) JoinOwnerResult {
	logs := []LogLine{}
	log := func(level, msg string) {
		logs = append(logs, LogLine{T: time.Now().UnixMilli(), Level: level, Msg: msg})
	}

	mgr, err := ParseSession(req.ManagerSession)
	if err != nil {
		return JoinOwnerResult{OK: false, Error: "母号 session: " + err.Error(), Logs: logs}
	}
	if mgr.AccountID == "" {
		return JoinOwnerResult{OK: false, Error: "母号 session 缺少 account.id（空间 UUID）", Logs: logs}
	}
	target, err := ParseSession(req.TargetSession)
	if err != nil {
		return JoinOwnerResult{OK: false, Error: "目标 session: " + err.Error(), Logs: logs}
	}

	setOwner := req.SetOwner
	// default true when using zero-value carefully — caller should set explicitly.
	// For web API we always pass the checkbox value.
	maxApprove := req.MaxApprove
	if maxApprove < 1 {
		maxApprove = 12
	}
	workspaceID := mgr.AccountID
	proxy := strings.TrimSpace(req.Proxy)

	log("info", fmt.Sprintf("母号 %s · workspace=%s", orDash(mgr.Email), truncID(workspaceID)))
	log("info", fmt.Sprintf("目标 %s · user=%s", orDash(target.Email), truncID(target.UserID)))
	if proxy != "" {
		log("info", "使用代理: "+maskProxy(proxy))
	} else {
		log("warn", "未配置代理（直连 chatgpt.com，可能被风控）")
	}

	// 1) Join request
	log("info", "步骤 1/3 · 加入空间 (request) …")
	join := Join(target.AccessToken, workspaceID, "request", proxy)
	if join.OK {
		log("ok", fmt.Sprintf("加入成功 · HTTP %d", join.StatusCode))
	} else {
		log("warn", fmt.Sprintf("加入结果 · HTTP %d · %s", join.StatusCode, join.Error))
	}

	// 2) Approve (+ owner)
	var approve *ApproveResult
	var owner *OwnerResult
	if target.Email == "" {
		log("warn", "目标无 email，跳过按邮箱审批")
		if setOwner {
			owner = &OwnerResult{OK: false, Error: "目标 session 缺少 email"}
		}
	} else {
		step := "审批通过"
		if setOwner {
			step = "审批并设为 owner"
		}
		log("info", fmt.Sprintf("步骤 2/3 · 母号%s · %s …", step, target.Email))
		ar := ApproveByEmailAsOwner(ManagerSession{
			Token:     mgr.AccessToken,
			AccountID: workspaceID,
			Email:     mgr.Email,
		}, target.Email, proxy, maxApprove, setOwner)
		approve = &ar
		if ar.OK {
			if setOwner {
				log("ok", fmt.Sprintf("审批+owner 成功 · invite=%s", truncID(ar.InviteID)))
				owner = &OwnerResult{OK: true, Role: "account-owner", InviteID: ar.InviteID, Via: "PATCH /invites/{id}"}
			} else {
				log("ok", fmt.Sprintf("审批成功 · invite=%s", truncID(ar.InviteID)))
			}
			time.Sleep(1500 * time.Millisecond)
		} else {
			log("err", "审批失败 · "+ar.Error)
			if setOwner {
				owner = &OwnerResult{OK: false, Error: ar.Error, Via: "PATCH /invites/{id}"}
			}
		}
	}

	// 3) accounts/check + session_after
	log("info", "步骤 3/3 · 拉取 accounts/check …")
	var plan *PlanInfo
	var checkRaw any
	if raw, p, e := AccountsCheck(target.AccessToken, proxy, workspaceID); e != nil {
		log("warn", "accounts/check: "+e.Error())
	} else {
		checkRaw = raw
		plan = &p
		log("ok", fmt.Sprintf("plan_type=%s account=%s role=%s", orDash(p.Plan), truncID(p.AccountID), orDash(p.Role)))
	}

	sessionAfter := BuildSessionAfter(target, workspaceID, join, approve, owner, plan, setOwner)

	ok := join.OK || (approve != nil && approve.OK) || (owner != nil && owner.OK) || (plan != nil && isTeamish(plan.Plan))
	res := JoinOwnerResult{
		OK:            ok,
		WorkspaceID:   workspaceID,
		Manager:       toSummary(mgr),
		Target:        toSummary(target),
		Join:          join,
		Approve:       approve,
		Owner:         owner,
		Plan:          plan,
		SessionAfter:  sessionAfter,
		AccountsCheck: checkRaw,
		Logs:          logs,
		ProxyUsed:     maskProxy(proxy),
	}
	if !ok && res.Error == "" {
		if approve != nil && !approve.OK {
			res.Error = approve.Error
		} else if !join.OK {
			res.Error = join.Error
		} else {
			res.Error = "未完全成功"
		}
	}
	return res
}

// ParseSession extracts accessToken / email / account id from session-like JSON.
func ParseSession(raw any) (ParsedSession, error) {
	var data map[string]any
	switch v := raw.(type) {
	case nil:
		return ParsedSession{}, fmt.Errorf("为空")
	case string:
		t := strings.TrimSpace(v)
		if t == "" {
			return ParsedSession{}, fmt.Errorf("为空")
		}
		if strings.HasPrefix(t, "eyJ") && strings.Count(t, ".") >= 2 {
			return ParsedSession{AccessToken: t, Raw: map[string]any{"accessToken": t}}, nil
		}
		if err := json.Unmarshal([]byte(t), &data); err != nil {
			return ParsedSession{}, fmt.Errorf("不是合法 JSON")
		}
	case map[string]any:
		data = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ParsedSession{}, fmt.Errorf("格式错误")
		}
		if err := json.Unmarshal(b, &data); err != nil {
			return ParsedSession{}, fmt.Errorf("格式错误")
		}
	}
	if data == nil {
		return ParsedSession{}, fmt.Errorf("格式错误")
	}

	token := strAny(data["accessToken"])
	if token == "" {
		token = strAny(data["access_token"])
	}
	if token == "" {
		token = strAny(data["token"])
	}
	if token == "" {
		if s, ok := data["session"].(map[string]any); ok {
			token = strAny(s["accessToken"])
			if token == "" {
				token = strAny(s["access_token"])
			}
		}
	}
	if token == "" {
		return ParsedSession{}, fmt.Errorf("未找到 accessToken")
	}

	user, _ := data["user"].(map[string]any)
	account, _ := data["account"].(map[string]any)
	// Nested session wrapper (some exports wrap auth/session payload).
	if account == nil {
		if s, ok := data["session"].(map[string]any); ok {
			if a, ok := s["account"].(map[string]any); ok {
				account = a
			}
			if user == nil {
				if u, ok := s["user"].(map[string]any); ok {
					user = u
				}
			}
		}
	}
	ps := ParsedSession{
		AccessToken: strings.TrimSpace(token),
		Email:       firstStr(strAny(user["email"]), strAny(data["email"])),
		UserID:      firstStr(strAny(user["id"]), strAny(data["user_id"]), strAny(data["userId"])),
		// Workspace UUID: always from session JSON — never a separate UI field.
		AccountID: firstStr(
			strAny(account["id"]),
			strAny(data["account_id"]),
			strAny(data["chatgpt_account_id"]),
			strAny(data["workspace_id"]),
			strAny(data["workspaceId"]),
			// last resort: top-level id only if it looks like a UUID (avoid mistaking other ids)
			uuidOrEmpty(strAny(data["id"])),
		),
		PlanType: firstStr(strAny(account["planType"]), strAny(account["plan_type"]), strAny(data["plan_type"])),
		Expires:  strAny(data["expires"]),
		Raw:      data,
	}
	return ps, nil
}

func uuidOrEmpty(s string) string {
	s = strings.TrimSpace(s)
	// rough UUID shape: 8-4-4-4-12
	if len(s) == 36 && strings.Count(s, "-") == 4 {
		return s
	}
	return ""
}

// ApproveByEmailAsOwner lists pending requests and PATCHes invite.
// When asOwner, body is {"role":"account-owner","seat_type":"default","accept_request":true}.
func ApproveByEmailAsOwner(mgr ManagerSession, email, proxy string, maxAttempts int, asOwner bool) ApproveResult {
	if maxAttempts < 1 {
		maxAttempts = 8
	}
	if maxAttempts > 12 {
		maxAttempts = 12
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return ApproveResult{Error: err.Error()}
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)
	email = strings.ToLower(strings.TrimSpace(email))
	if mgr.AccountID == "" {
		return ApproveResult{Error: "母号缺少 account id"}
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
					if !aliasMatch(email, got) {
						continue
					}
				}
				id := str(it["id"])
				if id == "" {
					continue
				}
				if err := patchInvite(client, mgr, id, asOwner); err != nil {
					return ApproveResult{OK: false, InviteID: id, Email: got, Error: err.Error()}
				}
				return ApproveResult{OK: true, InviteID: id, Email: got}
			}
		}
		if attempt+1 < maxAttempts {
			time.Sleep(approveBackoff(attempt))
		}
	}
	return ApproveResult{OK: false, Error: fmt.Sprintf("未找到待审批请求: %s", email)}
}

func patchInvite(client *httpx.Client, mgr ManagerSession, inviteID string, asOwner bool) error {
	path := fmt.Sprintf("/backend-api/accounts/%s/invites/%s", mgr.AccountID, inviteID)
	u := ChatGPTBase + path
	headers := managerHeaders(mgr, path)
	headers["x-openai-target-route"] = "/backend-api/accounts/{account_id}/invites/{invite_id}"
	headers["oai-language"] = "zh-CN"
	headers["accept-language"] = "zh-CN,zh;q=0.9"

	var payloads []map[string]any
	if asOwner {
		payloads = []map[string]any{
			{"role": "account-owner", "seat_type": "default", "accept_request": true},
			{"role": "account-owner", "accept_request": true},
			{"role": "account-owner"},
		}
	} else {
		payloads = []map[string]any{{"accept_request": true}}
	}

	var last error
	for _, p := range payloads {
		body, _ := json.Marshal(p)
		resp, err := client.Do("PATCH", u, body, headers, false)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("PATCH invite HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return last
		}
	}
	if last == nil {
		last = fmt.Errorf("patch invite failed")
	}
	return last
}

// AccountsCheck returns full accounts/check JSON + selected plan info.
func AccountsCheck(accessToken, proxy, preferredWorkspaceID string) (raw map[string]any, plan PlanInfo, err error) {
	client, err := httpx.New(proxy)
	if err != nil {
		return nil, plan, err
	}
	defer client.Close()

	headers := map[string]string{
		"authorization": "Bearer " + accessToken,
		"accept":        "*/*",
		"oai-language":  "zh-CN",
		"user-agent":    httpx.UserAgent,
	}
	if preferredWorkspaceID != "" {
		headers["chatgpt-account-id"] = preferredWorkspaceID
	}
	resp, err := client.Get(ChatGPTBase+"/backend-api/accounts/check/v4-2023-04-27", headers, false)
	if err != nil {
		return nil, plan, err
	}
	if resp.StatusCode != 200 {
		return nil, plan, fmt.Errorf("HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 120))
	}
	_ = json.Unmarshal(resp.Body, &raw)
	if raw == nil {
		raw = map[string]any{}
	}
	p, aid, role := pickPlanRole(raw, preferredWorkspaceID)
	plan = PlanInfo{Plan: p, AccountID: aid, Role: role}
	return raw, plan, nil
}

func pickPlanRole(data map[string]any, preferred string) (plan, accountID, role string) {
	accts, _ := data["accounts"].(map[string]any)
	type ent struct{ plan, id, role, key string }
	var entries []ent
	for k, v := range accts {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		acc, _ := m["account"].(map[string]any)
		p := str(acc["plan_type"])
		if p == "" {
			p = str(acc["planType"])
		}
		id := str(acc["account_id"])
		if id == "" {
			id = str(acc["id"])
		}
		if id == "" {
			id = k
		}
		r := str(m["account_user_role"])
		if r == "" {
			r = str(acc["account_user_role"])
		}
		entries = append(entries, ent{plan: p, id: id, role: r, key: k})
	}
	pref := strings.ToLower(strings.TrimSpace(preferred))
	for _, e := range entries {
		if pref != "" && strings.ToLower(e.id) == pref {
			return e.plan, e.id, e.role
		}
	}
	for _, e := range entries {
		if isTeamish(e.plan) {
			return e.plan, e.id, e.role
		}
	}
	if len(entries) > 0 {
		return entries[0].plan, entries[0].id, entries[0].role
	}
	return "", "", ""
}

// BuildSessionAfter merges target session with join/plan metadata into a
// chatgpt.com /api/auth/session–shaped JSON (accessToken + user + account + …).
func BuildSessionAfter(
	target ParsedSession,
	workspaceID string,
	join JoinResult,
	approve *ApproveResult,
	owner *OwnerResult,
	plan *PlanInfo,
	setOwner bool,
) map[string]any {
	// 1) Start from pasted browser session when present; else empty.
	base := map[string]any{}
	if target.Raw != nil {
		b, _ := json.Marshal(target.Raw)
		_ = json.Unmarshal(b, &base)
	}

	// 2) Normalize token field names to browser style.
	at := firstStr(strAny(base["accessToken"]), strAny(base["access_token"]), target.AccessToken)
	if at != "" {
		base["accessToken"] = at
	}
	// Drop non-session snake_case duplicates from register path.
	delete(base, "access_token")

	// 3) Fill user / expires from JWT when thin (register-built blob).
	claims := decodeJWTPayload(at)
	if idTok := firstStr(strAny(base["id_token"])); idTok != "" {
		// id_token often has richer profile than access token
		if more := decodeJWTPayload(idTok); len(more) > 0 {
			for k, v := range more {
				if _, ok := claims[k]; !ok {
					claims[k] = v
				}
			}
		}
	}

	accountID := workspaceID
	planType := target.PlanType
	role := ""
	if plan != nil {
		if plan.AccountID != "" {
			accountID = plan.AccountID
		}
		if plan.Plan != "" {
			planType = plan.Plan
		}
		role = plan.Role
	}

	// user block
	user, _ := base["user"].(map[string]any)
	if user == nil {
		user = map[string]any{}
	}
	email := firstStr(
		strAny(user["email"]),
		target.Email,
		strAny(base["email"]),
		jwtPathString(claims, "https://api.openai.com/profile", "email"),
		jwtString(claims, "email"),
	)
	userID := firstStr(
		strAny(user["id"]),
		target.UserID,
		jwtPathString(claims, "https://api.openai.com/auth", "user_id"),
		jwtPathString(claims, "https://api.openai.com/auth", "chatgpt_user_id"),
		jwtString(claims, "sub"),
	)
	name := firstStr(strAny(user["name"]), jwtPathString(claims, "https://api.openai.com/profile", "name"), jwtString(claims, "name"))
	if email != "" {
		user["email"] = email
	}
	if userID != "" {
		user["id"] = userID
	}
	if name != "" {
		user["name"] = name
	}
	if strAny(user["idp"]) == "" {
		user["idp"] = "auth0"
	}
	if _, ok := user["mfa"]; !ok {
		user["mfa"] = false
	}
	base["user"] = user

	// expires from JWT exp when missing
	if strAny(base["expires"]) == "" {
		if exp := jwtExpTime(claims); !exp.IsZero() {
			base["expires"] = exp.UTC().Format(time.RFC3339)
		} else if target.Expires != "" {
			base["expires"] = target.Expires
		}
	}

	// account block (workspace membership after join)
	acc, _ := base["account"].(map[string]any)
	if acc == nil {
		acc = map[string]any{}
	}
	if accountID != "" {
		acc["id"] = accountID
	}
	if planType != "" {
		acc["planType"] = planType
		acc["plan_type"] = planType
	}
	// Prefer existing structure from real session; register path → workspace.
	if strAny(acc["structure"]) == "" {
		if workspaceID != "" {
			acc["structure"] = "workspace"
		} else {
			acc["structure"] = "personal"
		}
	}
	// Common residency defaults seen in browser session (only fill if absent).
	if _, ok := acc["computeResidency"]; !ok {
		acc["computeResidency"] = "no_constraint"
	}
	if _, ok := acc["residencyRegion"]; !ok {
		acc["residencyRegion"] = "no_constraint"
	}
	base["account"] = acc

	// 4) Session chrome (browser parity)
	if strAny(base["WARNING_BANNER"]) == "" {
		base["WARNING_BANNER"] = SessionWarning
	}
	if strAny(base["authProvider"]) == "" {
		base["authProvider"] = "openai"
	}
	if _, ok := base["rumViewTags"]; !ok {
		base["rumViewTags"] = map[string]any{
			"light_account": map[string]any{"fetched": false},
		}
	}

	// 5) Join / approve / owner metadata (our pipeline fields)
	base["workspace_id"] = workspaceID
	base["chatgpt_account_id"] = accountID
	if join.OK {
		base["join_status"] = "ok"
	} else if join.Error != "" || join.StatusCode != 0 {
		base["join_status"] = "failed"
	}
	if approve != nil {
		if approve.OK {
			base["approve_status"] = "ok"
		} else {
			base["approve_status"] = "failed"
		}
	}
	if owner != nil {
		if owner.OK {
			base["role"] = firstStr(owner.Role, "account-owner")
			base["account_user_role"] = firstStr(role, owner.Role, "account-owner")
			base["owner_status"] = "ok"
		} else {
			base["owner_status"] = "failed"
		}
	} else if !setOwner {
		base["owner_status"] = "skipped"
	}
	if role != "" && strAny(base["account_user_role"]) == "" {
		base["account_user_role"] = role
	}
	if planType != "" {
		base["plan_type"] = planType
	}
	base["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)

	// 6) Keep useful register secrets, but out of the way of session shape.
	// password / refresh_token remain if present; strip internal-only noise.
	delete(base, "via")
	delete(base, "source_type")
	delete(base, "proxy_used")
	// top-level email is not in browser session (lives under user)
	if _, ok := base["user"].(map[string]any); ok {
		delete(base, "email")
	}
	// id_token is oauth, not browser session field
	delete(base, "id_token")

	return base
}

// decodeJWTPayload decodes JWT middle segment without verifying signature.
func decodeJWTPayload(tok string) map[string]any {
	tok = strings.TrimSpace(tok)
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	seg := parts[1]
	// raw URL-safe base64 without padding
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func jwtString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	return strAny(claims[key])
}

func jwtPathString(claims map[string]any, objKey, field string) string {
	if claims == nil {
		return ""
	}
	obj, _ := claims[objKey].(map[string]any)
	if obj == nil {
		return ""
	}
	return strAny(obj[field])
}

func jwtExpTime(claims map[string]any) time.Time {
	if claims == nil {
		return time.Time{}
	}
	switch v := claims["exp"].(type) {
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

func toSummary(p ParsedSession) SessionSummary {
	return SessionSummary{
		Email:     p.Email,
		UserID:    p.UserID,
		AccountID: p.AccountID,
		PlanType:  p.PlanType,
		Expires:   p.Expires,
	}
}

func strAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func firstStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 14 {
		return s[:12] + "…"
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func maskProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if u, err := url.Parse(p); err == nil && u.Host != "" {
		if u.User != nil {
			return u.Scheme + "://***:***@" + u.Host
		}
		return u.Scheme + "://" + u.Host
	}
	if len(p) > 24 {
		return p[:12] + "…"
	}
	return p
}
