package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"k12reg/internal/httpx"
)

const (
	TokenURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	GraphMsgsURL = "https://graph.microsoft.com/v1.0/me/messages"
	// Reseller / sub-account pools typically only work with .default
	// (returns Mail.ReadWrite). Mail.Read alone → AADSTS70000 invalid_grant.
	GraphScopeDefault = "https://graph.microsoft.com/.default"
	GraphScopeClassic = "offline_access https://graph.microsoft.com/Mail.Read"
)

// graphScopes tried in order until token exchange succeeds.
var graphScopes = []string{
	GraphScopeDefault,
	"offline_access https://graph.microsoft.com/.default",
	GraphScopeClassic,
}

// Mailbox is one Outlook pool entry (possibly a plus-alias).
type Mailbox struct {
	Address      string
	BaseEmail    string
	Password     string
	ClientID     string
	RefreshToken string
}

// Pool is a simple sequential mailbox allocator with disk state.
type Pool struct {
	mu            sync.Mutex
	items         []Mailbox
	statePath     string
	state         map[string]string // email_lower -> state
	index         int
	requireDomain string // if set, only acquire emails on this domain
}

// SetRequireDomain restricts Acquire to addresses on domain (e.g. "hotmail.com").
func (p *Pool) SetRequireDomain(domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requireDomain = strings.ToLower(strings.TrimSpace(domain))
}

// Available counts free mailboxes (optionally domain-filtered).
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, mb := range p.items {
		key := strings.ToLower(mb.Address)
		st := p.state[key]
		if st == "used" || st == "failed" || st == "token_invalid" || st == "in_use" {
			continue
		}
		if p.requireDomain != "" && emailDomain(mb.Address) != p.requireDomain {
			continue
		}
		n++
	}
	return n
}

func emailDomain(email string) string {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return ""
	}
	return d
}

// Graph access-token cache (per base refresh_token) — OTP polls every ~1.5s.
type cachedAT struct {
	token string
	exp   time.Time
}

var graphTokenCache sync.Map // key clientID|rt → cachedAT

type Credential struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
}

func ParseCredentials(text string) []Credential {
	var out []Credential
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || !strings.Contains(line, "----") {
			continue
		}
		parts := strings.SplitN(line, "----", 4)
		if len(parts) != 4 {
			continue
		}
		// Canonical pool format (Python mail_provider):
		//   email----password----refresh_token----client_id
		// Also accept email----password----client_id----refresh_token.
		email := strings.TrimSpace(parts[0])
		password := strings.TrimSpace(parts[1])
		a := strings.TrimSpace(parts[2])
		b := strings.TrimSpace(parts[3])
		if !strings.Contains(email, "@") || a == "" || b == "" {
			continue
		}
		var clientID, refresh string
		switch {
		case looksGUID(b) && looksRefresh(a):
			// email----pwd----refresh----client_id
			refresh, clientID = a, b
		case looksGUID(a) && looksRefresh(b):
			// email----pwd----client_id----refresh
			clientID, refresh = a, b
		case looksGUID(b):
			refresh, clientID = a, b
		case looksGUID(a):
			clientID, refresh = a, b
		default:
			// fallback: prefer longer field as refresh token
			if len(a) >= len(b) {
				refresh, clientID = a, b
			} else {
				clientID, refresh = a, b
			}
		}
		key := strings.ToLower(email)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Credential{Email: email, Password: password, ClientID: clientID, RefreshToken: refresh})
	}
	return out
}

func looksGUID(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 32 && strings.Count(s, "-") == 4
}

func looksRefresh(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) > 100 || strings.HasPrefix(s, "M.C") || strings.HasPrefix(s, "0.A") || strings.HasPrefix(s, "Ew")
}

func expandAliases(creds []Credential, aliasCount int) []Mailbox {
	if aliasCount < 1 {
		aliasCount = 1
	}
	var out []Mailbox
	for _, c := range creds {
		if aliasCount <= 1 {
			out = append(out, Mailbox{
				Address: c.Email, BaseEmail: c.Email,
				Password: c.Password, ClientID: c.ClientID, RefreshToken: c.RefreshToken,
			})
			continue
		}
		local, domain, ok := strings.Cut(c.Email, "@")
		if !ok {
			continue
		}
		local = strings.Split(local, "+")[0]
		sum := sha256.Sum256([]byte(strings.ToLower(local)))
		tag := hex.EncodeToString(sum[:])[:4]
		for i := 1; i <= aliasCount; i++ {
			alias := fmt.Sprintf("%s+%s%d@%s", local, tag, i, domain)
			out = append(out, Mailbox{
				Address: alias, BaseEmail: c.Email,
				Password: c.Password, ClientID: c.ClientID, RefreshToken: c.RefreshToken,
			})
		}
	}
	return out
}

func LoadPool(mailboxesFile, statePath string, aliasCount int) (*Pool, error) {
	data, err := os.ReadFile(mailboxesFile)
	if err != nil {
		return nil, err
	}
	creds := ParseCredentials(string(data))
	if len(creds) == 0 {
		return nil, fmt.Errorf("no outlook credentials in %s", mailboxesFile)
	}
	p := &Pool{
		items:     expandAliases(creds, aliasCount),
		statePath: statePath,
		state:     map[string]string{},
	}
	p.loadState()
	return p, nil
}

func (p *Pool) loadState() {
	if p.statePath == "" {
		return
	}
	b, err := os.ReadFile(p.statePath)
	if err != nil {
		return
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			p.state[strings.ToLower(k)] = t
		case map[string]any:
			if s, ok := t["state"].(string); ok {
				p.state[strings.ToLower(k)] = s
			}
		}
	}
}

func (p *Pool) saveState() {
	if p.statePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.statePath), 0o755)
	out := map[string]map[string]string{}
	for k, st := range p.state {
		out[k] = map[string]string{"state": st, "updated_at": time.Now().UTC().Format(time.RFC3339)}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	_ = os.WriteFile(p.statePath, append(b, '\n'), 0o644)
}

// Acquire next available mailbox.
func (p *Pool) Acquire() (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(p.items); i++ {
		idx := (p.index + i) % len(p.items)
		mb := p.items[idx]
		key := strings.ToLower(mb.Address)
		st := p.state[key]
		if st == "used" || st == "failed" || st == "token_invalid" || st == "in_use" {
			continue
		}
		if p.requireDomain != "" && emailDomain(mb.Address) != p.requireDomain {
			continue
		}
		p.state[key] = "in_use"
		p.index = idx + 1
		p.saveState()
		return mb, nil
	}
	msg := fmt.Sprintf("outlook pool exhausted (%d entries)", len(p.items))
	if p.requireDomain != "" {
		msg += fmt.Sprintf(" for domain @%s", p.requireDomain)
	}
	return Mailbox{}, fmt.Errorf("%s", msg)
}

func (p *Pool) Mark(mb Mailbox, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(mb.Address)
	if success {
		p.state[key] = "used"
	} else {
		p.state[key] = "failed"
	}
	p.saveState()
}

func (p *Pool) Release(mb Mailbox) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(mb.Address)
	if p.state[key] == "in_use" {
		delete(p.state, key)
		p.saveState()
	}
}

// claimedOTPMsgs: message-ref → {code, at}. Aligns with Python's _seen_code_message_refs:
// two alias waiters share one Graph inbox; each claims a distinct mail, not a bare 6-digit.
var claimedOTPMsgs sync.Map // msgRef -> claimedOTPEntry

type claimedOTPEntry struct {
	Code string
	At   time.Time
}

func purgeStaleOTPClaims() {
	now := time.Now()
	claimedOTPMsgs.Range(func(k, v any) bool {
		if e, ok := v.(claimedOTPEntry); ok && now.Sub(e.At) > 15*time.Minute {
			claimedOTPMsgs.Delete(k)
		}
		return true
	})
}

func claimOTPMessage(msgRef, code string) bool {
	msgRef = strings.TrimSpace(msgRef)
	code = strings.TrimSpace(code)
	if msgRef == "" || code == "" {
		return false
	}
	purgeStaleOTPClaims()
	_, loaded := claimedOTPMsgs.LoadOrStore(msgRef, claimedOTPEntry{Code: code, At: time.Now()})
	return !loaded
}

// UnclaimOTPCode releases claims for a code if OpenAI rejected it (wrong/expired),
// so another waiter (or retry) may try the same message again.
func UnclaimOTPCode(code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		return
	}
	claimedOTPMsgs.Range(func(k, v any) bool {
		if e, ok := v.(claimedOTPEntry); ok && e.Code == code {
			claimedOTPMsgs.Delete(k)
		}
		return true
	})
}

// messageOTPRef uniquely identifies one Graph message for claim tracking.
func messageOTPRef(m graphMsg, code string) string {
	if m.ID != "" {
		return "id:" + m.ID
	}
	// Fallback fingerprint when Graph omits id.
	h := sha256.Sum256([]byte(strings.ToLower(m.Subject) + "|" + m.Received.UTC().Format(time.RFC3339Nano) + "|" + code + "|" + strings.Join(m.Recipients, ",")))
	return "fp:" + hex.EncodeToString(h[:12])
}

// recipientMatchesMailbox mirrors Python _message_recipient_matches:
// plus-aliases share one inbox; only accept mail addressed to this alias.
// Empty recipient list → accept (some messages omit To) to avoid deadlock.
func recipientMatchesMailbox(mb Mailbox, m graphMsg) bool {
	address := strings.ToLower(strings.TrimSpace(mb.Address))
	base := strings.ToLower(strings.TrimSpace(mb.BaseEmail))
	if address == "" || address == base || (base == "" && !strings.Contains(address, "+")) {
		return true // not an alias
	}
	local, _, _ := strings.Cut(address, "@")
	tag := ""
	if i := strings.Index(local, "+"); i >= 0 {
		tag = local[i+1:]
	}
	if len(m.Recipients) == 0 {
		return true
	}
	blob := strings.ToLower(strings.Join(m.Recipients, " "))
	if strings.Contains(blob, address) {
		return true
	}
	return tag != "" && strings.Contains(blob, "+"+tag)
}

var otpCodeRes = []*regexp.Regexp{
	regexp.MustCompile(`(?is)background-color:\s*#F3F3F3[^>]*>[\s\S]*?(\d{6})[\s\S]*?</p>`),
	regexp.MustCompile(`(?i)(?:Verification code|Your code is|code is|代码为|验证码)[:\s]*(\d{6})`),
	regexp.MustCompile(`(?i)temporary\s+openai\s+verification\s+code[\s\S]{0,80}?(\d{6})`),
	regexp.MustCompile(`(?i)>\s*(\d{6})\s*<`),
}

func extractOTPCode(m graphMsg) string {
	blob := m.Subject + "\n" + m.Preview + "\n" + m.Body
	for _, re := range otpCodeRes {
		if sub := re.FindStringSubmatch(blob); len(sub) > 1 {
			return sub[1]
		}
	}
	return ""
}

// WaitForCode polls Graph for a 6-digit OpenAI OTP.
// notBefore: only accept messages received at/after this time (set right after send-OTP).
// onTick is optional progress callback: elapsed, timeout, note.
// ctx cancellation aborts quickly (between Graph polls / sleeps).
func WaitForCode(ctx context.Context, mb Mailbox, timeout, interval time.Duration, notBefore time.Time, onTick func(elapsed, total time.Duration, note string)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := httpx.New("")
	if err != nil {
		return "", err
	}
	defer client.Close()
	// Shorter per-request timeout so Stop doesn't wait up to 90s on a hung Graph call.
	if client.Session != nil {
		client.Session.SetTimeout(20 * time.Second)
	}
	// Abort in-flight Graph HTTP as soon as Stop cancels ctx.
	if stop := context.AfterFunc(ctx, func() { client.Close() }); stop != nil {
		defer stop()
	}

	start := time.Now().UTC()
	deadline := start.Add(timeout)
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	// Strict boundary (Python _code_not_before): only tiny skew for Graph clock vs local.
	if notBefore.IsZero() {
		notBefore = start
	} else {
		notBefore = notBefore.UTC()
	}
	notBefore = notBefore.Add(-500 * time.Millisecond)

	attempt := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		attempt++
		elapsed := time.Since(start)
		msgs, err := fetchGraph(client, mb)
		if err != nil {
			// Permanent Graph auth failures: fail fast (don't spam until OTP timeout).
			es := strings.ToLower(err.Error())
			if strings.Contains(es, "aadsts70000") || strings.Contains(es, "compromised") ||
				strings.Contains(es, "aadsts50196") || strings.Contains(es, "request loop") {
				return "", fmt.Errorf("graph auth failed: %w", err)
			}
			if onTick != nil {
				onTick(elapsed, timeout, fmt.Sprintf("graph err · try %d · %v", attempt, err))
			}
			if err := sleepCtx(ctx, interval); err != nil {
				return "", err
			}
			continue
		}

		type cand struct {
			code string
			ref  string
			msg  graphMsg
		}
		var cands []cand
		skippedSibling := 0
		for _, m := range msgs {
			if !m.Received.IsZero() && m.Received.Before(notBefore) {
				continue
			}
			if !recipientMatchesMailbox(mb, m) {
				if looksLikeOpenAIOTP(m) {
					skippedSibling++
				}
				continue
			}
			if !looksLikeOpenAIOTP(m) {
				continue
			}
			code := extractOTPCode(m)
			if code == "" {
				continue
			}
			ref := messageOTPRef(m, code)
			cands = append(cands, cand{code: code, ref: ref, msg: m})
		}

		// Newest first (Graph usually already desc, but re-sort to be sure).
		for i := 0; i < len(cands); i++ {
			for j := i + 1; j < len(cands); j++ {
				ti, tj := cands[i].msg.Received, cands[j].msg.Received
				if tj.After(ti) {
					cands[i], cands[j] = cands[j], cands[i]
				}
			}
		}

		// Claim by message-ref so concurrent aliases each take their own mail.
		for _, c := range cands {
			if !claimOTPMessage(c.ref, c.code) {
				continue
			}
			if onTick != nil {
				age := ""
				if !c.msg.Received.IsZero() {
					age = fmt.Sprintf(" · age=%0.0fs", time.Since(c.msg.Received).Seconds())
				}
				onTick(elapsed, timeout, fmt.Sprintf("matched · %s%s", trunc(c.msg.Subject, 40), age))
			}
			return c.code, nil
		}

		if onTick != nil {
			note := fmt.Sprintf("inbox msgs=%d cand=%d claimed", len(msgs), len(cands))
			if skippedSibling > 0 {
				note += fmt.Sprintf(" · skip_sibling=%d", skippedSibling)
			}
			onTick(elapsed, timeout, note)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("otp timeout after %s for %s", timeout, mb.Address)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func looksLikeOpenAIOTP(m graphMsg) bool {
	from := strings.ToLower(m.From)
	sub := strings.ToLower(m.Subject)
	prev := strings.ToLower(m.Preview)
	blob := from + " " + sub + " " + prev
	if strings.Contains(from, "openai") || strings.Contains(from, "chatgpt") {
		return true
	}
	if strings.Contains(blob, "openai") || strings.Contains(blob, "chatgpt") {
		return true
	}
	if strings.Contains(sub, "verification") || strings.Contains(sub, "verify") ||
		strings.Contains(sub, "验证") || strings.Contains(sub, "one-time") {
		return true
	}
	return false
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type graphMsg struct {
	ID         string
	Subject    string
	Body       string
	Preview    string
	From       string
	Recipients []string // to + cc addresses (lowercased)
	Received   time.Time
}

func exchangeAccessToken(client *httpx.Client, mb Mailbox) (string, error) {
	cacheKey := mb.ClientID + "|" + mb.RefreshToken
	if v, ok := graphTokenCache.Load(cacheKey); ok {
		c := v.(cachedAT)
		if time.Now().Before(c.exp) && c.token != "" {
			return c.token, nil
		}
	}

	var lastErr error
	for _, scope := range graphScopes {
		form := url.Values{}
		form.Set("client_id", mb.ClientID)
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", mb.RefreshToken)
		form.Set("scope", scope)
		tokResp, err := client.PostForm(TokenURL, form, map[string]string{
			"User-Agent": httpx.UserAgent,
		})
		if err != nil {
			lastErr = err
			continue
		}
		var tok map[string]any
		_ = json.Unmarshal(tokResp.Body, &tok)
		at, _ := tok["access_token"].(string)
		if tokResp.StatusCode == 200 && at != "" {
			// cache ~45m (Graph tokens typically 60–90m)
			graphTokenCache.Store(cacheKey, cachedAT{token: at, exp: time.Now().Add(45 * time.Minute)})
			return at, nil
		}
		desc, _ := tok["error_description"].(string)
		if desc == "" {
			desc = httpx.DumpSnippet(tokResp.Body, 160)
		}
		lastErr = fmt.Errorf("token refresh HTTP %d scope=%q: %s", tokResp.StatusCode, scope, desc)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("token refresh failed")
	}
	return "", lastErr
}

func fetchGraph(client *httpx.Client, mb Mailbox) ([]graphMsg, error) {
	at, err := exchangeAccessToken(client, mb)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	// Higher top when aliases share one inbox (many concurrent OTPs).
	q.Set("$top", "40")
	q.Set("$orderby", "receivedDateTime desc")
	// toRecipients/ccRecipients: required to disambiguate plus-aliases (Python parity).
	q.Set("$select", "id,subject,receivedDateTime,from,toRecipients,ccRecipients,body,bodyPreview")
	resp, err := client.Get(GraphMsgsURL+"?"+q.Encode(), map[string]string{
		"Authorization": "Bearer " + at,
		"Accept":        "application/json",
		"User-Agent":    httpx.UserAgent,
	}, false)
	if err != nil {
		return nil, err
	}
	type graphAddr struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}
	var data struct {
		Value []struct {
			ID               string `json:"id"`
			Subject          string `json:"subject"`
			ReceivedDateTime string `json:"receivedDateTime"`
			BodyPreview      string `json:"bodyPreview"`
			Body             struct {
				ContentType string `json:"contentType"`
				Content     string `json:"content"`
			} `json:"body"`
			From         graphAddr   `json:"from"`
			ToRecipients []graphAddr `json:"toRecipients"`
			CcRecipients []graphAddr `json:"ccRecipients"`
		} `json:"value"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, err
	}
	var out []graphMsg
	for _, it := range data.Value {
		var rt time.Time
		if t, err := time.Parse(time.RFC3339, it.ReceivedDateTime); err == nil {
			rt = t
		}
		var rcpts []string
		for _, r := range it.ToRecipients {
			if a := strings.ToLower(strings.TrimSpace(r.EmailAddress.Address)); a != "" {
				rcpts = append(rcpts, a)
			}
		}
		for _, r := range it.CcRecipients {
			if a := strings.ToLower(strings.TrimSpace(r.EmailAddress.Address)); a != "" {
				rcpts = append(rcpts, a)
			}
		}
		out = append(out, graphMsg{
			ID:         it.ID,
			Subject:    it.Subject,
			Body:       it.Body.Content,
			Preview:    it.BodyPreview,
			From:       it.From.EmailAddress.Address,
			Recipients: rcpts,
			Received:   rt,
		})
	}
	return out, nil
}
