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
	// Reseller 子邮箱 (seller): only Graph .default — never Mail.Read.
	// Cascading scopes multiplies failed login.microsoftonline.com hits and
	// can trip AADSTS70000 "compromised" / security interrupt under concurrency.
	GraphScopeDefault = "https://graph.microsoft.com/.default"
)

// graphScopes: seller Graph only. Do not add Mail.Read / IMAP scopes here.
var graphScopes = []string{
	GraphScopeDefault,
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
		if !p.entryUsableLocked(mb) {
			continue
		}
		n++
	}
	return n
}

// entryUsableLocked: address free AND base not Graph-dead / fully burned.
func (p *Pool) entryUsableLocked(mb Mailbox) bool {
	key := strings.ToLower(mb.Address)
	st := p.state[key]
	if st == "used" || st == "failed" || st == "token_invalid" || st == "in_use" {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(mb.BaseEmail))
	if base == "" {
		base = key
	}
	// Only Graph-dead / explicit base burn blocks all aliases.
	// Per-alias success no longer sets base=used (see Mark).
	switch p.state[base] {
	case "used", "token_invalid":
		return false
	}
	if p.requireDomain != "" && emailDomain(mb.Address) != p.requireDomain {
		return false
	}
	return true
}

func mailboxBase(mb Mailbox) string {
	base := strings.ToLower(strings.TrimSpace(mb.BaseEmail))
	if base == "" {
		return strings.ToLower(strings.TrimSpace(mb.Address))
	}
	return base
}

// baseBusyLocked is true if any alias of this base is currently in_use
// (another worker is mid-registration on the same Graph inbox).
func (p *Pool) baseBusyLocked(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return false
	}
	for _, item := range p.items {
		if mailboxBase(item) != base {
			continue
		}
		if p.state[strings.ToLower(item.Address)] == "in_use" {
			return true
		}
	}
	// Base itself may be marked in_use in edge cases.
	return p.state[base] == "in_use"
}

func emailDomain(email string) string {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return ""
	}
	return d
}

// Graph access-token cache (per base refresh_token) — OTP polls every ~1.5s.
// Singleflight + negative cache: concurrent aliases of one base share one exchange
// (Python _cached_access_token), instead of N× scope attempts that trip AADSTS.
type cachedAT struct {
	token string
	exp   time.Time
	err   error // sticky permanent failure until exp
}

var (
	graphTokenCache sync.Map // key clientID|rt → cachedAT
	graphTokenMu    sync.Map // key → *sync.Mutex singleflight
)

func tokenCacheKey(mb Mailbox) string {
	return mb.ClientID + "|" + mb.RefreshToken
}

func tokenFlightMu(key string) *sync.Mutex {
	v, _ := graphTokenMu.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

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
//
// Preference (for high concurrency + plus-aliases):
//  1. Free alias whose base has NO other in_use worker (different Graph inbox).
//     e.g. 20 threads → 20 different base prefixes when stock allows.
//  2. Else any free alias (may share a base only when all free bases are busy).
//
// Scan still walks pool order so stock is consumed somewhat evenly.
func (p *Pool) Acquire() (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Pass 1: prefer bases that are not currently busy (spread across inboxes).
	if mb, ok := p.acquirePassLocked(true); ok {
		return mb, nil
	}
	// Pass 2: any free alias (fallback when concurrent > free bases).
	if mb, ok := p.acquirePassLocked(false); ok {
		return mb, nil
	}
	return Mailbox{}, p.exhaustedErr()
}

// preferIdleBase: only take aliases of bases with zero in_use siblings.
func (p *Pool) acquirePassLocked(preferIdleBase bool) (Mailbox, bool) {
	n := len(p.items)
	if n == 0 {
		return Mailbox{}, false
	}
	for i := 0; i < n; i++ {
		idx := (p.index + i) % n
		mb := p.items[idx]
		if !p.entryUsableLocked(mb) {
			continue
		}
		if preferIdleBase && p.baseBusyLocked(mailboxBase(mb)) {
			continue
		}
		key := strings.ToLower(mb.Address)
		p.state[key] = "in_use"
		p.index = idx + 1
		p.saveState()
		return mb, true
	}
	return Mailbox{}, false
}

// AcquireFromEnd takes the last free mailbox in the pool file order
// (end of hotmail.txt first). Used by one-shot flows that should burn
// stock from the tail without aliases.
func (p *Pool) AcquireFromEnd() (Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.items) - 1; i >= 0; i-- {
		mb := p.items[i]
		if !p.entryUsableLocked(mb) {
			continue
		}
		key := strings.ToLower(mb.Address)
		p.state[key] = "in_use"
		// Keep sequential index consistent if mixed with Acquire later.
		p.index = i
		p.saveState()
		return mb, nil
	}
	return Mailbox{}, p.exhaustedErr()
}

func (p *Pool) exhaustedErr() error {
	msg := fmt.Sprintf("outlook pool exhausted (%d entries)", len(p.items))
	if p.requireDomain != "" {
		msg += fmt.Sprintf(" for domain @%s", p.requireDomain)
	}
	return fmt.Errorf("%s", msg)
}

func (p *Pool) Mark(mb Mailbox, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(mb.Address)
	base := mailboxBase(mb)
	if !success {
		// Soft-fail: free this alias only (sibling aliases unaffected).
		if p.state[key] == "in_use" || p.state[key] == "failed" {
			delete(p.state, key)
		}
		p.saveState()
		return
	}
	// Success: burn this alias only — other plus-aliases of the same base stay usable.
	// (Graph-dead still burns whole base via MarkGraphDead.)
	p.state[key] = "used"
	p.saveState()
	_ = base // base no longer auto-burned on success
}

// SeedUsed marks emails as used without acquiring (e.g. already have session
// files or rows in registered_accounts). Returns how many newly marked.
func (p *Pool) SeedUsed(emails []string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range emails {
		key := strings.ToLower(strings.TrimSpace(e))
		if key == "" || !strings.Contains(key, "@") {
			continue
		}
		cur := p.state[key]
		if cur == "used" || cur == "token_invalid" {
			continue
		}
		p.state[key] = "used"
		n++
	}
	if n > 0 {
		p.saveState()
	}
	return n
}

// MarkUsed forces address + base to used (consumed stock).
func (p *Pool) MarkUsed(mb Mailbox) {
	p.Mark(mb, true)
}

// MarkGraphDead marks mailbox as used (consumed) when Graph token is permanently dead
// (AADSTS70000 compromised / invalid_grant / etc.). Same base + same refresh_token
// aliases are all marked used so the pool will not pick them again — treated like
// "this mailbox stock was already burned / already used".
func (p *Pool) MarkGraphDead(mb Mailbox) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(mb.Address)
	base := strings.ToLower(strings.TrimSpace(mb.BaseEmail))
	if base == "" {
		base = key
	}
	mark := func(addr string) {
		if addr == "" {
			return
		}
		// Do not downgrade a successful used; force used for free/in_use/failed/token_invalid.
		p.state[addr] = "used"
	}
	mark(key)
	mark(base)
	for _, item := range p.items {
		addr := strings.ToLower(item.Address)
		itemBase := strings.ToLower(strings.TrimSpace(item.BaseEmail))
		if itemBase == "" {
			itemBase = addr
		}
		sameBase := itemBase == base || addr == base || itemBase == key
		sameRT := mb.RefreshToken != "" && item.RefreshToken == mb.RefreshToken
		if sameBase || sameRT {
			mark(addr)
			mark(itemBase)
		}
	}
	p.saveState()
}

// MarkTokenInvalid is kept as an alias of MarkGraphDead for clarity at call sites.
func (p *Pool) MarkTokenInvalid(mb Mailbox) { p.MarkGraphDead(mb) }

// IsGraphAuthPermanent reports token/account deaths that must not be retried.
func IsGraphAuthPermanent(err error) bool {
	if err == nil {
		return false
	}
	es := strings.ToLower(err.Error())
	for _, k := range []string{
		"aadsts70000", "compromised", "aadsts50196", "request loop",
		"aadsts70008", "aadsts700084", "invalid_grant", "token has been revoked",
		"user account is found as compromised", "security interrupt",
		"graph auth failed",
	} {
		if strings.Contains(es, k) {
			return true
		}
	}
	return false
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
	// Graph polls: fail faster than default OpenAI timeout.
	client.SetTimeout(httpx.GraphTimeout)
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
	cacheKey := tokenCacheKey(mb)
	if v, ok := graphTokenCache.Load(cacheKey); ok {
		c := v.(cachedAT)
		if time.Now().Before(c.exp) {
			if c.token != "" {
				return c.token, nil
			}
			if c.err != nil {
				return "", c.err
			}
		}
	}

	// Singleflight: one refresh per (client_id, refresh_token) at a time.
	mu := tokenFlightMu(cacheKey)
	mu.Lock()
	defer mu.Unlock()

	// Re-check after wait (another alias may have filled cache).
	if v, ok := graphTokenCache.Load(cacheKey); ok {
		c := v.(cachedAT)
		if time.Now().Before(c.exp) {
			if c.token != "" {
				return c.token, nil
			}
			if c.err != nil {
				return "", c.err
			}
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
		// Permanent account/token death: do not try more scopes (and never Mail.Read).
		if IsGraphAuthPermanent(lastErr) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("token refresh failed")
	}
	// Negative cache so concurrent aliases don't hammer login.microsoftonline.com.
	ttl := 2 * time.Minute
	if IsGraphAuthPermanent(lastErr) {
		ttl = 30 * time.Minute
	}
	graphTokenCache.Store(cacheKey, cachedAT{err: lastErr, exp: time.Now().Add(ttl)})
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
