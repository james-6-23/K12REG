package register

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	mrand "math/rand"
	"net/url"
	"strings"
	"time"

	"k12reg/internal/httpx"
	"k12reg/internal/mail"
	"k12reg/internal/pkce"
	"k12reg/internal/sentinel"
)

func jsonBody(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b
}

const (
	AuthBase     = "https://auth.openai.com"
	PlatformBase = "https://platform.openai.com"
	ClientID     = "app_2SKx67EdpoN0G6j64rFvigXD"
	RedirectURI  = PlatformBase + "/auth/callback"
	Audience     = "https://api.openai.com/v1"
	Auth0Client  = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
)

// Result of a successful protocol registration.
type Result struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	SourceType   string `json:"source_type"`
	CreatedAt    string `json:"created_at"`
}

type Options struct {
	Proxy           string
	Mailbox         mail.Mailbox
	OTPTimeout      time.Duration
	OTPInterval     time.Duration
	SentinelVMDir   string
	Log             func(string)
	Ctx             context.Context
}

func (opt Options) ctx() context.Context {
	if opt.Ctx != nil {
		return opt.Ctx
	}
	return context.Background()
}

func (opt Options) errIfDone() error {
	return opt.ctx().Err()
}

func logf(opt Options, format string, args ...any) {
	if opt.Log != nil {
		opt.Log(fmt.Sprintf(format, args...))
	}
}

// Run protocol registration for one mailbox.
func Run(opt Options) (*Result, error) {
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	client, err := httpx.New(opt.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	// 30s per request (was 90s) — proxy half-open no longer freezes a whole worker.
	client.SetTimeout(httpx.DefaultTimeout)
	// Stop button cancels ctx → close TLS session to abort hung OpenAI/Graph HTTP.
	if stop := context.AfterFunc(opt.ctx(), func() { client.Close() }); stop != nil {
		defer stop()
	}

	deviceID := randomUUID()
	client.SetCookie("oai-did", deviceID, "auth.openai.com")
	client.SetCookie("oai-did", deviceID, ".auth.openai.com")

	password := randomPassword(16)
	first, last := randomName()
	email := opt.Mailbox.Address

	logf(opt, "authorize PKCE signup · %s", email)
	verifier, challenge, err := pkce.Generate()
	if err != nil {
		return nil, err
	}
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	if err := platformAuthorize(client, email, deviceID, challenge); err != nil {
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "register user · %s %s", first, last)
	if err := registerUser(client, email, password, deviceID, opt.SentinelVMDir); err != nil {
		return nil, err
	}

	// Plus-aliases share one Graph inbox; WaitForCode filters by To/Cc (Python parity)
	// so concurrent aliases of the same base can wait in parallel without inbox lock.
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "send OTP")
	if err := sendOTP(client); err != nil {
		return nil, err
	}
	// Boundary for inbox scan: only messages after we requested the code.
	otpSentAt := time.Now().UTC()

	timeout := opt.OTPTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	interval := opt.OTPInterval
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	logf(opt, "OTP · waiting · timeout=%s", timeout)
	var lastTick time.Time
	var lastNote string
	code, err := mail.WaitForCode(opt.ctx(), opt.Mailbox, timeout, interval, otpSentAt, func(elapsed, total time.Duration, note string) {
		// Throttle noisy scan lines; always print matches / errors.
		interesting := strings.Contains(note, "matched") || strings.Contains(note, "graph err")
		now := time.Now()
		if !interesting && note == lastNote && now.Sub(lastTick) < 10*time.Second {
			return
		}
		if !interesting && now.Sub(lastTick) < 8*time.Second {
			return
		}
		lastTick = now
		lastNote = note
		logf(opt, "OTP · %0.0f/%0.0fs · %s", elapsed.Seconds(), total.Seconds(), note)
	})
	if err != nil {
		return nil, err
	}
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "OTP · got %s · validate", code)
	if err := validateOTP(client, code, deviceID, opt.SentinelVMDir); err != nil {
		// Allow another waiter to try this code if OpenAI rejects it (rare).
		mail.UnclaimOTPCode(code)
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	fullName := first + " " + last
	birth := randomBirthdate()
	logf(opt, "create account · %s · dob %s", fullName, birth)
	authCode, err := createAccount(client, fullName, birth, deviceID, opt.SentinelVMDir)
	if err != nil {
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "exchange tokens")
	tokens, err := exchangeTokens(client, verifier, authCode)
	if err != nil {
		return nil, err
	}

	return &Result{
		Email:        email,
		Password:     password,
		AccessToken:  tokens["access_token"],
		RefreshToken: tokens["refresh_token"],
		IDToken:      tokens["id_token"],
		SourceType:   "web",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func platformAuthorize(client *httpx.Client, email, deviceID, challenge string) error {
	q := url.Values{}
	q.Set("issuer", AuthBase)
	q.Set("client_id", ClientID)
	q.Set("audience", Audience)
	q.Set("redirect_uri", RedirectURI)
	q.Set("device_id", deviceID)
	q.Set("screen_hint", "signup")
	q.Set("max_age", "0")
	q.Set("login_hint", email)
	q.Set("scope", "openid profile email offline_access")
	q.Set("response_type", "code")
	q.Set("response_mode", "query")
	q.Set("state", randomURLSafe(32))
	q.Set("nonce", randomURLSafe(32))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("auth0Client", Auth0Client)
	target := AuthBase + "/api/accounts/authorize?" + q.Encode()
	headers := navigateHeaders(PlatformBase + "/")
	resp, err := client.Get(target, headers, true)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("platform_authorize HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 300))
	}
	return nil
}

func registerUser(client *httpx.Client, email, password, deviceID, vmDir string) error {
	headers := jsonHeaders(AuthBase+"/create-account/password", deviceID)
	b, err := sentinel.Build(client, deviceID, "username_password_create", vmDir)
	if err != nil {
		return fmt.Errorf("sentinel: %w", err)
	}
	headers["openai-sentinel-token"] = b.Token
	if b.SOToken != "" {
		headers["openai-sentinel-so-token"] = b.SOToken
	}
	payload := jsonBody(map[string]string{"username": email, "password": password})
	resp, err := client.PostJSON(AuthBase+"/api/accounts/user/register", payload, headers, false)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("user_register HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 300))
	}
	return nil
}

func sendOTP(client *httpx.Client) error {
	headers := navigateHeaders(AuthBase + "/create-account/password")
	resp, err := client.Get(AuthBase+"/api/accounts/email-otp/send", headers, true)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 302 {
		return fmt.Errorf("send_otp HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
	}
	return nil
}

func validateOTP(client *httpx.Client, code, deviceID, vmDir string) error {
	headers := jsonHeaders(AuthBase+"/email-verification", deviceID)
	payload := jsonBody(map[string]string{"code": code})
	resp, err := client.PostJSON(AuthBase+"/api/accounts/email-otp/validate", payload, headers, false)
	if err != nil {
		return err
	}
	if resp.StatusCode == 200 {
		return nil
	}
	// retry with sentinel
	b, err := sentinel.Build(client, deviceID, "authorize_continue", vmDir)
	if err == nil {
		headers["openai-sentinel-token"] = b.Token
		if b.SOToken != "" {
			headers["openai-sentinel-so-token"] = b.SOToken
		}
		resp, err = client.PostJSON(AuthBase+"/api/accounts/email-otp/validate", payload, headers, false)
		if err != nil {
			return err
		}
		if resp.StatusCode == 200 {
			return nil
		}
	}
	return fmt.Errorf("validate_otp HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
}

func createAccount(client *httpx.Client, name, birth, deviceID, vmDir string) (string, error) {
	// Warm about-you page (browser does this before the POST).
	_, _ = client.Get(AuthBase+"/about-you", navigateHeaders(AuthBase+"/email-verification"), true)

	headers := jsonHeaders(AuthBase+"/about-you", deviceID)
	b, err := sentinel.Build(client, deviceID, "oauth_create_account", vmDir)
	if err != nil {
		return "", fmt.Errorf("sentinel: %w", err)
	}
	headers["openai-sentinel-token"] = b.Token
	if b.SOToken != "" {
		headers["openai-sentinel-so-token"] = b.SOToken
	}
	// Log sentinel shape for diagnosis (no full secrets).
	var smap map[string]string
	_ = json.Unmarshal([]byte(b.Token), &smap)
	_ = smap // used only if we log below via empty check

	payload := jsonBody(map[string]string{"name": name, "birthdate": birth})
	resp, err := client.PostJSON(AuthBase+"/api/accounts/create_account", payload, headers, false)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 302 && resp.StatusCode != 303 {
		tLen, soLen := 0, 0
		if smap != nil {
			tLen = len(smap["t"])
		}
		soLen = len(b.SOToken)
		return "", fmt.Errorf("create_account HTTP %d (sentinel t=%d so=%d): %s",
			resp.StatusCode, tLen, soLen, httpx.DumpSnippet(resp.Body, 300))
	}
	continueURL := ""
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if u, ok := body["continue_url"].(string); ok {
		continueURL = u
	}
	if continueURL == "" {
		continueURL = resp.HeaderGet("Location")
	}
	code := extractOAuthCode(continueURL)
	if code == "" {
		return "", fmt.Errorf("no auth code in create_account (status=%d url=%s)", resp.StatusCode, continueURL)
	}
	return code, nil
}

func exchangeTokens(client *httpx.Client, verifier, code string) (map[string]string, error) {
	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9",
		"auth0-client":       Auth0Client,
		"content-type":       "application/json",
		"origin":             PlatformBase,
		"referer":            PlatformBase + "/",
		"sec-ch-ua":          httpx.SecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-site",
		"user-agent":         httpx.UserAgent,
	}
	payload := jsonBody(map[string]string{
		"client_id":     ClientID,
		"code_verifier": verifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  RedirectURI,
	})
	resp, err := client.PostJSON(AuthBase+"/api/accounts/oauth/token", payload, headers, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 300))
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	out := map[string]string{}
	for _, k := range []string{"access_token", "refresh_token", "id_token"} {
		if v, ok := data[k].(string); ok {
			out[k] = v
		}
	}
	if out["access_token"] == "" {
		return nil, fmt.Errorf("token exchange missing access_token")
	}
	return out, nil
}

func extractOAuthCode(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("code"))
}

func jsonHeaders(referer, deviceID string) map[string]string {
	h := map[string]string{
		"accept":                          "application/json",
		"accept-encoding":                 "gzip, deflate, br",
		"accept-language":                 "en-US,en;q=0.9",
		"cache-control":                   "no-cache",
		"Content-Type":                    "application/json",
		"origin":                          "https://auth.openai.com",
		"referer":                         referer,
		"oai-device-id":                    deviceID,
		"sec-ch-ua":                       httpx.SecChUA,
		"sec-ch-ua-arch":                  `"x86_64"`,
		"sec-ch-ua-bitness":               `"64"`,
		"sec-ch-ua-full-version-list":     `"Chromium";v="145.0.0.0", "Not:A-Brand";v="99.0.0.0", "Google Chrome";v="145.0.0.0"`,
		"sec-ch-ua-mobile":                "?0",
		"sec-ch-ua-model":                 `""`,
		"sec-ch-ua-platform":              `"Windows"`,
		"sec-ch-ua-platform-version":      `"10.0.0"`,
		"sec-fetch-dest":                  "empty",
		"sec-fetch-mode":                  "cors",
		"sec-fetch-site":                  "same-origin",
		"user-agent":                      httpx.UserAgent,
	}
	// Datadog RUM headers (OpenAI auth expects these on JSON APIs)
	for k, v := range datadogHeaders() {
		h[k] = v
	}
	return h
}

func datadogHeaders() map[string]string {
	// 128-bit trace id hex + 64-bit parent
	trace := randomUUID()
	parent := fmt.Sprintf("%016x", mrand.Uint64())
	return map[string]string{
		"traceparent":                 fmt.Sprintf("00-%s-%s-01", strings.ReplaceAll(trace, "-", ""), parent[:16]),
		"tracestate":                  "dd=s:1;o:rum",
		"x-datadog-origin":            "rum",
		"x-datadog-parent-id":         fmt.Sprintf("%d", mrand.Uint64()),
		"x-datadog-sampling-priority": "1",
		"x-datadog-trace-id":          fmt.Sprintf("%d", mrand.Uint64()),
	}
}

func navigateHeaders(referer string) map[string]string {
	h := map[string]string{
		"accept":             "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"accept-language":    "en-US,en;q=0.9",
		"cache-control":      "max-age=0",
		"sec-ch-ua":          httpx.SecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "document",
		"sec-fetch-mode":     "navigate",
		"sec-fetch-site":     "same-site",
		"sec-fetch-user":     "?1",
		"upgrade-insecure-requests": "1",
		"user-agent":         httpx.UserAgent,
	}
	if referer != "" {
		h["referer"] = referer
	}
	return h
}

func randomPassword(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%"
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[v.Int64()]
	}
	// ensure classes
	b[0] = 'A'
	b[1] = 'a'
	b[2] = '1'
	b[3] = '!'
	return string(b)
}

func randomName() (string, string) {
	first := []string{"James", "Robert", "John", "Michael", "David", "Mary", "Emma", "Olivia"}
	last := []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller"}
	return first[mrand.Intn(len(first))], last[mrand.Intn(len(last))]
}

func randomBirthdate() string {
	y := 1996 + mrand.Intn(11)
	m := 1 + mrand.Intn(12)
	d := 1 + mrand.Intn(28)
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func randomURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
