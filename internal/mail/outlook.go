package mail

import (
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

// Base inbox lock: plus-aliases share one Graph mailbox; serialize sendOTP+wait
// so concurrent workers do not steal each other's verification codes.
var inboxOTPLocks sync.Map // baseEmail lower -> *sync.Mutex

// claimedOTPCodes prevents two waiters from returning the same 6-digit code.
var claimedOTPCodes sync.Map // code -> claimedAt

// LockInboxOTP serializes OTP send/wait for a physical inbox (BaseEmail).
// Call unlock when done (typically defer unlock()).
func LockInboxOTP(mb Mailbox) (unlock func()) {
	key := baseInboxKey(mb)
	v, _ := inboxOTPLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func baseInboxKey(mb Mailbox) string {
	b := strings.TrimSpace(mb.BaseEmail)
	if b == "" {
		b = mb.Address
	}
	// Collapse plus-alias to local@domain for safety.
	local, domain, ok := strings.Cut(strings.ToLower(b), "@")
	if ok {
		local = strings.Split(local, "+")[0]
		return local + "@" + domain
	}
	return strings.ToLower(b)
}

func claimOTPCode(code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	now := time.Now()
	// Drop codes older than 15 minutes.
	claimedOTPCodes.Range(func(k, v any) bool {
		if t, ok := v.(time.Time); ok && now.Sub(t) > 15*time.Minute {
			claimedOTPCodes.Delete(k)
		}
		return true
	})
	_, loaded := claimedOTPCodes.LoadOrStore(code, now)
	return !loaded
}

// UnclaimOTPCode releases a code if OpenAI rejected it (wrong/expired).
func UnclaimOTPCode(code string) {
	claimedOTPCodes.Delete(strings.TrimSpace(code))
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
func WaitForCode(mb Mailbox, timeout, interval time.Duration, notBefore time.Time, onTick func(elapsed, total time.Duration, note string)) (string, error) {
	client, err := httpx.New("")
	if err != nil {
		return "", err
	}
	defer client.Close()

	start := time.Now()
	deadline := start.Add(timeout)
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	// Tight window: concurrent aliases must not pick older OTPs for other sessions.
	if notBefore.IsZero() {
		notBefore = start.Add(-3 * time.Second)
	} else {
		notBefore = notBefore.Add(-3 * time.Second)
	}
	// Also ignore codes that arrived long before we started waiting (safety).
	if notBefore.Before(start.Add(-30 * time.Second)) {
		notBefore = start.Add(-30 * time.Second)
	}

	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		elapsed := time.Since(start)
		msgs, err := fetchGraph(client, mb)
		if err != nil {
			if onTick != nil {
				onTick(elapsed, timeout, fmt.Sprintf("graph err · try %d · %v", attempt, err))
			}
			time.Sleep(interval)
			continue
		}

		type cand struct {
			code string
			msg  graphMsg
		}
		var cands []cand
		for _, m := range msgs {
			if !m.Received.IsZero() && m.Received.Before(notBefore) {
				continue
			}
			if !looksLikeOpenAIOTP(m) {
				continue
			}
			code := extractOTPCode(m)
			if code == "" {
				continue
			}
			cands = append(cands, cand{code: code, msg: m})
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

		// Prefer unclaimed codes (other alias workers may have claimed older ones).
		for _, c := range cands {
			if !claimOTPCode(c.code) {
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
			onTick(elapsed, timeout, fmt.Sprintf("inbox msgs=%d cand=%d unclaimed=0", len(msgs), len(cands)))
		}
		time.Sleep(interval)
	}
	return "", fmt.Errorf("otp timeout after %s for %s", timeout, mb.Address)
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
	Subject  string
	Body     string
	Preview  string
	From     string
	Received time.Time
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
	q.Set("$top", "25")
	q.Set("$orderby", "receivedDateTime desc")
	q.Set("$select", "subject,receivedDateTime,from,body,bodyPreview")
	resp, err := client.Get(GraphMsgsURL+"?"+q.Encode(), map[string]string{
		"Authorization": "Bearer " + at,
		"Accept":        "application/json",
		"User-Agent":    httpx.UserAgent,
	}, false)
	if err != nil {
		return nil, err
	}
	var data struct {
		Value []struct {
			Subject           string `json:"subject"`
			ReceivedDateTime  string `json:"receivedDateTime"`
			BodyPreview       string `json:"bodyPreview"`
			Body              struct {
				ContentType string `json:"contentType"`
				Content     string `json:"content"`
			} `json:"body"`
			From struct {
				EmailAddress struct {
					Address string `json:"address"`
				} `json:"emailAddress"`
			} `json:"from"`
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
		body := it.Body.Content
		if strings.EqualFold(it.Body.ContentType, "html") && it.BodyPreview != "" {
			// keep both
		}
		out = append(out, graphMsg{
			Subject:  it.Subject,
			Body:     body,
			Preview:  it.BodyPreview,
			From:     it.From.EmailAddress.Address,
			Received: rt,
		})
	}
	return out, nil
}
