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

// OAuth paths selectable via settings (registration.oauth_path).
const (
	// PathChatGPTWeb: ChatGPT Web client (app_X8z…) + NextAuth session redeem.
	// JWT can carry model.* scopes; after join can elevate to k12 claims via session.
	PathChatGPTWeb = "chatgpt_web"
	// PathPlatform: legacy Platform OAuth (app_2SK…) + oauth/token PKCE only.
	// Simpler; JWT stays free-looking (no workspace claims in token).
	PathPlatform = "platform"
)

// ChatGPT Web OAuth (from Proxyman capture of real chatgpt.com signup).
// Platform client (app_2SK…) only yields basic scopes and free-looking JWT claims.
const (
	AuthBase    = "https://auth.openai.com"
	ChatGPTBase = "https://chatgpt.com"
	// Legacy Platform OAuth.
	PlatformBase     = "https://platform.openai.com"
	PlatformClientID = "app_2SKx67EdpoN0G6j64rFvigXD"
	PlatformRedirect = PlatformBase + "/auth/callback"
	PlatformScope    = "openid profile email offline_access"

	// Web client (default path).
	ClientID    = "app_X8zY6vW2pQ9tR3dE7nK1jL5gH"
	RedirectURI = ChatGPTBase + "/api/auth/callback/openai"
	Audience    = "https://api.openai.com/v1"
	// Full Web scopes — yields model.* + organization.* in the access token.
	Scope = "openid email profile offline_access model.request model.read organization.read organization.write"

	Auth0Client = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
)

// NormalizeOAuthPath maps user/settings values to a known path constant.
func NormalizeOAuthPath(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", PathChatGPTWeb, "web", "chatgpt", "chatgpt-web", "app_x8z":
		return PathChatGPTWeb
	case PathPlatform, "platform_oauth", "app_2sk", "legacy", "old":
		return PathPlatform
	default:
		return PathChatGPTWeb
	}
}

// Result of a successful protocol registration.
type Result struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	SessionToken string `json:"session_token,omitempty"`
	SourceType   string `json:"source_type"`
	CreatedAt    string `json:"created_at"`
}

type Options struct {
	Proxy         string
	Mailbox       mail.Mailbox
	OTPTimeout    time.Duration
	OTPInterval   time.Duration
	SentinelVMDir string
	Log           func(string)
	Ctx           context.Context
	// OAuthPath: PathChatGPTWeb (default) or PathPlatform. See registration.oauth_path.
	OAuthPath string
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
// Path is selected by Options.OAuthPath (settings: registration.oauth_path).
func Run(opt Options) (*Result, error) {
	path := NormalizeOAuthPath(opt.OAuthPath)
	switch path {
	case PathPlatform:
		return runPlatform(opt)
	default:
		return runChatGPTWeb(opt)
	}
}

// runChatGPTWeb: ChatGPT Web client + NextAuth session (Proxyman-aligned).
func runChatGPTWeb(opt Options) (*Result, error) {
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	client, err := httpx.New(opt.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)
	if stop := context.AfterFunc(opt.ctx(), func() { client.Close() }); stop != nil {
		defer stop()
	}

	deviceID := randomUUID()
	for _, dom := range []string{"auth.openai.com", ".auth.openai.com", "chatgpt.com", ".chatgpt.com"} {
		client.SetCookie("oai-did", deviceID, dom)
	}

	// Optional password (legacy re-login). Web capture is passwordless; we still
	// try password register first, then fall back to passwordless OTP signup.
	password := randomPassword(16)
	first, last := randomName()
	email := opt.Mailbox.Address

	logf(opt, "path=chatgpt_web · authorize · %s", email)
	verifier, challenge, err := pkce.Generate()
	if err != nil {
		return nil, err
	}
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	if err := chatgptAuthorize(client, email, deviceID, challenge); err != nil {
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	// Password path (older protocol). Web capture skips this (passwordless_signup).
	logf(opt, "register user (password path) · %s %s", first, last)
	usedPassword := true
	if err := registerUser(client, email, password, deviceID, opt.SentinelVMDir); err != nil {
		logf(opt, "password register skip · %v · try passwordless", err)
		usedPassword = false
		password = "" // no password set
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "send OTP")
	if err := sendOTP(client); err != nil {
		// Passwordless + login_hint often auto-sends OTP on authorize; ignore soft fail.
		logf(opt, "send OTP soft-fail · %v · continue (may already be sent)", err)
	}
	otpSentAt := time.Now().UTC()

	otpCode, err := waitOTP(opt, otpSentAt)
	if err != nil {
		return nil, err
	}
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "OTP · got %s · validate", otpCode)
	if err := validateOTP(client, otpCode, deviceID, opt.SentinelVMDir); err != nil {
		mail.UnclaimOTPCode(otpCode)
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	fullName := first + " " + last
	birth := randomBirthdate()
	logf(opt, "create account · %s · dob %s", fullName, birth)
	continueURL, authCode, err := createAccount(client, fullName, birth, deviceID, opt.SentinelVMDir)
	if err != nil {
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	// Prefer NextAuth callback → /api/auth/session (matches real browser).
	// This is critical: without session_token the pipeline cannot elevate the
	// AT JWT to chatgpt_plan_type=k12 after join+approve (check API alone is not enough).
	logf(opt, "redeem session via chatgpt callback")
	tokens, err := redeemViaSession(client, continueURL)
	if err != nil {
		logf(opt, "session redeem fail · %v · try oauth/token", err)
		tokens, err = exchangeTokens(client, verifier, authCode, ClientID, RedirectURI, ChatGPTBase)
		if err != nil {
			return nil, err
		}
		// oauth/token path usually has no NextAuth cookie. Best-effort bootstrap:
		// follow homepage → if auth cookies SSO, may still mint a session.
		if tokens["session_token"] == "" {
			if st, bootErr := bootstrapSessionToken(client); bootErr == nil && st != "" {
				tokens["session_token"] = st
				logf(opt, "session bootstrap ok · ST=yes")
			} else if bootErr != nil {
				logf(opt, "session bootstrap skip · %v", bootErr)
			}
		}
	} else if tokens["session_token"] == "" {
		// JSON omitted sessionToken — cookie harvest already tried inside redeem.
		logf(opt, "session redeem ok but ST empty · elevate may fail")
	} else {
		logf(opt, "session redeem ok · ST=yes")
	}

	src := "chatgpt_web"
	if usedPassword {
		src = "chatgpt_web_password"
	}
	return &Result{
		Email:        email,
		Password:     password,
		AccessToken:  tokens["access_token"],
		RefreshToken: tokens["refresh_token"],
		IDToken:      tokens["id_token"],
		SessionToken: tokens["session_token"],
		SourceType:   src,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// runPlatform: legacy Platform OAuth only (app_2SK… + PKCE + oauth/token).
// No ChatGPT Web session upgrade — JWT stays free-looking; membership via join/check.
// Kept for raw settings / advanced use; UI no longer exposes this path.
func runPlatform(opt Options) (*Result, error) {
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	client, err := httpx.New(opt.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	client.SetTimeout(httpx.DefaultTimeout)
	if stop := context.AfterFunc(opt.ctx(), func() { client.Close() }); stop != nil {
		defer stop()
	}

	deviceID := randomUUID()
	client.SetCookie("oai-did", deviceID, "auth.openai.com")
	client.SetCookie("oai-did", deviceID, ".auth.openai.com")

	password := randomPassword(16)
	first, last := randomName()
	email := opt.Mailbox.Address

	logf(opt, "path=platform · authorize PKCE signup · %s", email)
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

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "send OTP")
	if err := sendOTPPlatform(client); err != nil {
		return nil, err
	}
	otpSentAt := time.Now().UTC()

	otpCode, err := waitOTP(opt, otpSentAt)
	if err != nil {
		return nil, err
	}
	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "OTP · got %s · validate", otpCode)
	if err := validateOTP(client, otpCode, deviceID, opt.SentinelVMDir); err != nil {
		mail.UnclaimOTPCode(otpCode)
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	fullName := first + " " + last
	birth := randomBirthdate()
	logf(opt, "create account · %s · dob %s", fullName, birth)
	_, authCode, err := createAccount(client, fullName, birth, deviceID, opt.SentinelVMDir)
	if err != nil {
		return nil, err
	}

	if err := opt.errIfDone(); err != nil {
		return nil, err
	}
	logf(opt, "exchange tokens (platform oauth)")
	tokens, err := exchangeTokens(client, verifier, authCode, PlatformClientID, PlatformRedirect, PlatformBase)
	if err != nil {
		return nil, err
	}

	return &Result{
		Email:        email,
		Password:     password,
		AccessToken:  tokens["access_token"],
		RefreshToken: tokens["refresh_token"],
		IDToken:      tokens["id_token"],
		SourceType:   "platform",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func waitOTP(opt Options, otpSentAt time.Time) (string, error) {
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
	return mail.WaitForCode(opt.ctx(), opt.Mailbox, timeout, interval, otpSentAt, func(elapsed, total time.Duration, note string) {
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
}

// platformAuthorize is the legacy Platform SPA OAuth authorize step.
func platformAuthorize(client *httpx.Client, email, deviceID, challenge string) error {
	q := url.Values{}
	q.Set("issuer", AuthBase)
	q.Set("client_id", PlatformClientID)
	q.Set("audience", Audience)
	q.Set("redirect_uri", PlatformRedirect)
	q.Set("device_id", deviceID)
	q.Set("screen_hint", "signup")
	q.Set("max_age", "0")
	q.Set("login_hint", email)
	q.Set("scope", PlatformScope)
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

func sendOTPPlatform(client *httpx.Client) error {
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

// bootstrapSessionToken tries to mint a NextAuth session cookie when we only
// have oauth/token. Works when auth.openai.com SSO cookies are still live.
func bootstrapSessionToken(client *httpx.Client) (string, error) {
	_, _ = client.Get(ChatGPTBase+"/", navigateHeaders(""), true)
	// If cookie already present after callback residue:
	if st := client.Cookie(ChatGPTBase+"/", "__Secure-next-auth.session-token"); st != "" {
		return st, nil
	}
	csrf := ""
	if resp, err := client.Get(ChatGPTBase+"/api/auth/csrf", map[string]string{
		"accept": "application/json", "referer": ChatGPTBase + "/", "user-agent": httpx.UserAgent,
	}, false); err == nil && resp.StatusCode == 200 {
		var body map[string]any
		_ = json.Unmarshal(resp.Body, &body)
		csrf, _ = body["csrfToken"].(string)
	}
	if csrf == "" {
		return "", fmt.Errorf("empty csrf")
	}
	form := url.Values{}
	form.Set("callbackUrl", ChatGPTBase+"/")
	form.Set("csrfToken", csrf)
	form.Set("json", "true")
	q := url.Values{}
	q.Set("prompt", "none") // use existing SSO if any
	q.Set("ext-passkey-client-capabilities", "01111")
	resp, err := client.Post(ChatGPTBase+"/api/auth/signin/openai?"+q.Encode(), []byte(form.Encode()), map[string]string{
		"accept": "*/*", "content-type": "application/x-www-form-urlencoded",
		"origin": ChatGPTBase, "referer": ChatGPTBase + "/", "user-agent": httpx.UserAgent,
	}, false)
	if err != nil {
		return "", err
	}
	authURL := ""
	if resp.StatusCode == 200 {
		var body map[string]any
		_ = json.Unmarshal(resp.Body, &body)
		authURL, _ = body["url"].(string)
	}
	if authURL != "" {
		_, _ = client.Get(authURL, navigateHeaders(ChatGPTBase+"/"), true)
	}
	if st := client.Cookie(ChatGPTBase+"/", "__Secure-next-auth.session-token"); st != "" {
		return st, nil
	}
	if st := client.Cookie(ChatGPTBase+"/", "next-auth.session-token"); st != "" {
		return st, nil
	}
	return "", fmt.Errorf("no session cookie after SSO bootstrap")
}

// chatgptAuthorize mirrors chatgpt.com NextAuth sign-in → authorize.
// Falls back to a direct authorize hit if signin bootstrap fails.
func chatgptAuthorize(client *httpx.Client, email, deviceID, challenge string) error {
	// Warm chatgpt origin (csrf/cookies).
	_, _ = client.Get(ChatGPTBase+"/", navigateHeaders(""), true)

	csrf := ""
	if resp, err := client.Get(ChatGPTBase+"/api/auth/csrf", map[string]string{
		"accept":     "application/json",
		"referer":    ChatGPTBase + "/",
		"user-agent": httpx.UserAgent,
	}, false); err == nil && resp.StatusCode == 200 {
		var body map[string]any
		_ = json.Unmarshal(resp.Body, &body)
		csrf, _ = body["csrfToken"].(string)
	}

	authURL := ""
	if csrf != "" {
		q := url.Values{}
		q.Set("prompt", "login")
		q.Set("ext-passkey-client-capabilities", "01111")
		q.Set("ext-oai-did", deviceID)
		q.Set("auth_session_logging_id", randomUUID())
		q.Set("screen_hint", "login_or_signup")
		q.Set("login_hint", email)
		form := url.Values{}
		form.Set("callbackUrl", ChatGPTBase+"/")
		form.Set("csrfToken", csrf)
		form.Set("json", "true")
		headers := map[string]string{
			"accept":       "*/*",
			"content-type": "application/x-www-form-urlencoded",
			"origin":       ChatGPTBase,
			"referer":      ChatGPTBase + "/",
			"user-agent":   httpx.UserAgent,
		}
		resp, err := client.Post(ChatGPTBase+"/api/auth/signin/openai?"+q.Encode(), []byte(form.Encode()), headers, false)
		if err == nil && resp.StatusCode == 200 {
			var body map[string]any
			_ = json.Unmarshal(resp.Body, &body)
			authURL, _ = body["url"].(string)
		}
	}

	if authURL == "" {
		// Direct authorize (still Web client + full scopes). Include PKCE for oauth/token fallback.
		authURL = buildAuthorizeURL(email, deviceID, challenge, true)
	}

	headers := navigateHeaders(ChatGPTBase + "/")
	resp, err := client.Get(authURL, headers, true)
	if err != nil {
		return err
	}
	// 200 landing on create-account / email-verification / password is OK.
	if resp.StatusCode != 200 && resp.StatusCode != 302 {
		return fmt.Errorf("chatgpt_authorize HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 300))
	}
	return nil
}

func buildAuthorizeURL(email, deviceID, challenge string, withPKCE bool) string {
	q := url.Values{}
	q.Set("client_id", ClientID)
	q.Set("scope", Scope)
	q.Set("response_type", "code")
	q.Set("redirect_uri", RedirectURI)
	q.Set("audience", Audience)
	q.Set("device_id", deviceID)
	q.Set("prompt", "login")
	q.Set("ext-passkey-client-capabilities", "01111")
	q.Set("ext-oai-did", deviceID)
	q.Set("auth_session_logging_id", randomUUID())
	q.Set("screen_hint", "login_or_signup")
	q.Set("login_hint", email)
	q.Set("ccaps", "login_methods")
	q.Set("state", randomURLSafe(32))
	if withPKCE && challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("response_mode", "query")
		q.Set("auth0Client", Auth0Client)
	}
	return AuthBase + "/api/accounts/authorize?" + q.Encode()
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
	// Web often lands on /email-verification; password path uses create-account/password.
	headers := navigateHeaders(AuthBase + "/email-verification")
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

func createAccount(client *httpx.Client, name, birth, deviceID, vmDir string) (continueURL, authCode string, err error) {
	_, _ = client.Get(AuthBase+"/about-you", navigateHeaders(AuthBase+"/email-verification"), true)

	headers := jsonHeaders(AuthBase+"/about-you", deviceID)
	b, err := sentinel.Build(client, deviceID, "oauth_create_account", vmDir)
	if err != nil {
		return "", "", fmt.Errorf("sentinel: %w", err)
	}
	headers["openai-sentinel-token"] = b.Token
	if b.SOToken != "" {
		headers["openai-sentinel-so-token"] = b.SOToken
	}
	var smap map[string]string
	_ = json.Unmarshal([]byte(b.Token), &smap)

	payload := jsonBody(map[string]string{"name": name, "birthdate": birth})
	resp, err := client.PostJSON(AuthBase+"/api/accounts/create_account", payload, headers, false)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 302 && resp.StatusCode != 303 {
		tLen, soLen := 0, 0
		if smap != nil {
			tLen = len(smap["t"])
		}
		soLen = len(b.SOToken)
		return "", "", fmt.Errorf("create_account HTTP %d (sentinel t=%d so=%d): %s",
			resp.StatusCode, tLen, soLen, httpx.DumpSnippet(resp.Body, 300))
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	if u, ok := body["continue_url"].(string); ok {
		continueURL = u
	}
	if continueURL == "" {
		continueURL = resp.HeaderGet("Location")
	}
	authCode = extractOAuthCode(continueURL)
	if continueURL == "" || authCode == "" {
		return "", "", fmt.Errorf("no continue_url/code in create_account (status=%d url=%s)", resp.StatusCode, continueURL)
	}
	return continueURL, authCode, nil
}

// redeemViaSession follows the NextAuth callback (sets session cookies) then
// GET /api/auth/session for accessToken — same shape as browser ChatGPT.
// Also harvests __Secure-next-auth.session-token from the cookie jar so the
// pipeline can elevate to a K12 workspace-scoped JWT after join+approve.
func redeemViaSession(client *httpx.Client, continueURL string) (map[string]string, error) {
	if continueURL == "" {
		return nil, fmt.Errorf("empty continue_url")
	}
	headers := navigateHeaders(AuthBase + "/")
	headers["sec-fetch-site"] = "cross-site"
	resp, err := client.Get(continueURL, headers, true)
	if err != nil {
		return nil, fmt.Errorf("callback: %w", err)
	}
	// 200/302 both fine after redirects into chatgpt.com
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("callback HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
	}

	// Warm chatgpt origin once more — helps NextAuth cookie settle.
	_, _ = client.Get(ChatGPTBase+"/", navigateHeaders(ChatGPTBase+"/"), true)

	sessHeaders := map[string]string{
		"accept":     "application/json",
		"referer":    ChatGPTBase + "/",
		"origin":     ChatGPTBase,
		"user-agent": httpx.UserAgent,
	}
	var data map[string]any
	var lastBody []byte
	var lastStatus int
	for attempt := 1; attempt <= 4; attempt++ {
		resp, err = client.Get(ChatGPTBase+"/api/auth/session", sessHeaders, false)
		if err != nil {
			if attempt == 4 {
				return nil, fmt.Errorf("auth/session: %w", err)
			}
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
			continue
		}
		lastStatus = resp.StatusCode
		lastBody = resp.Body
		if resp.StatusCode != 200 {
			if attempt < 4 && (resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500) {
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("auth/session HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
		}
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			if attempt == 4 {
				return nil, fmt.Errorf("auth/session json: %w", err)
			}
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
			continue
		}
		break
	}
	if data == nil {
		return nil, fmt.Errorf("auth/session HTTP %d: %s", lastStatus, httpx.DumpSnippet(lastBody, 200))
	}
	out := map[string]string{}
	if v, ok := data["accessToken"].(string); ok {
		out["access_token"] = strings.TrimSpace(v)
	}
	if v, ok := data["access_token"].(string); ok && out["access_token"] == "" {
		out["access_token"] = strings.TrimSpace(v)
	}
	if v, ok := data["sessionToken"].(string); ok {
		out["session_token"] = strings.TrimSpace(v)
	}
	if v, ok := data["session_token"].(string); ok && out["session_token"] == "" {
		out["session_token"] = strings.TrimSpace(v)
	}
	// Cookie jar is the durable session source for later K12 elevate.
	if st := client.Cookie(ChatGPTBase+"/", "__Secure-next-auth.session-token"); st != "" {
		out["session_token"] = st
	} else if st := client.Cookie(ChatGPTBase+"/", "next-auth.session-token"); st != "" {
		out["session_token"] = st
	}
	// Optional account fields from session JSON.
	if acc, ok := data["account"].(map[string]any); ok {
		if id, _ := acc["id"].(string); strings.TrimSpace(id) != "" {
			out["chatgpt_account_id"] = strings.TrimSpace(id)
		}
		if p, _ := acc["planType"].(string); strings.TrimSpace(p) != "" {
			out["plan_type"] = strings.ToLower(strings.TrimSpace(p))
		}
	}
	if out["access_token"] == "" {
		return nil, fmt.Errorf("auth/session missing accessToken: %s", httpx.DumpSnippet(lastBody, 240))
	}
	return out, nil
}

func exchangeTokens(client *httpx.Client, verifier, code, clientID, redirectURI, origin string) (map[string]string, error) {
	if origin == "" {
		origin = ChatGPTBase
	}
	if clientID == "" {
		clientID = ClientID
	}
	if redirectURI == "" {
		redirectURI = RedirectURI
	}
	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9",
		"auth0-client":       Auth0Client,
		"content-type":       "application/json",
		"origin":             origin,
		"referer":            origin + "/",
		"sec-ch-ua":          httpx.SecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-site",
		"user-agent":         httpx.UserAgent,
	}
	payload := jsonBody(map[string]string{
		"client_id":     clientID,
		"code_verifier": verifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
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
	for k, v := range datadogHeaders() {
		h[k] = v
	}
	return h
}

func datadogHeaders() map[string]string {
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
		"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"accept-language":           "en-US,en;q=0.9",
		"cache-control":             "max-age=0",
		"sec-ch-ua":                 httpx.SecChUA,
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        `"Windows"`,
		"sec-fetch-dest":            "document",
		"sec-fetch-mode":            "navigate",
		"sec-fetch-site":            "same-site",
		"sec-fetch-user":            "?1",
		"upgrade-insecure-requests": "1",
		"user-agent":                httpx.UserAgent,
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
