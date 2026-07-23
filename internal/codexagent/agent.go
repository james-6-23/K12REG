// Package codexagent registers Codex CLI Agent Identity from a ChatGPT session JWT.
// Ported from Tool/codex-agent/codex_agent.py (by 久雾).
//
// Flow:
//  1. Decode access_token JWT for account_id / user_id / email / plan
//  2. Generate Ed25519 keypair
//  3. POST auth.openai.com/api/accounts/v1/agent/register
//  4. Optionally verify via task/register (signature)
//  5. Emit Codex CLI auth.json (auth_mode=agent_identity)
package codexagent

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k12reg/internal/httpx"
)

const (
	AuthAPIBase = "https://auth.openai.com/api/accounts"

	// Codex CLI agent metadata (must match what Codex CLI sends).
	AgentVersion    = "0.138.0-alpha.6"
	AgentHarnessID  = "codex-cli"
	RunningLocation = "local"
)

// AuthJSON is the Codex CLI ~/.codex/auth.json shape for agent_identity mode.
type AuthJSON struct {
	AuthMode      string        `json:"auth_mode"`
	AgentIdentity AgentIdentity `json:"agent_identity"`
}

// AgentIdentity holds the registered agent credentials.
type AgentIdentity struct {
	AgentRuntimeID          string `json:"agent_runtime_id"`
	AgentPrivateKey         string `json:"agent_private_key"` // PKCS8 DER base64
	AccountID               string `json:"account_id"`
	ChatGPTUserID           string `json:"chatgpt_user_id"`
	Email                   string `json:"email"`
	PlanType                string `json:"plan_type"`
	ChatGPTAccountIsFedramp bool   `json:"chatgpt_account_is_fedramp"`
}

// Options for Create.
type Options struct {
	AccessToken string
	Proxy       string
	// VerifyTask registers a signed task after agent registration (optional).
	VerifyTask bool
}

// SessionInfo is the subset of JWT claims needed for auth.json.
type SessionInfo struct {
	AccessToken string
	AccountID   string
	UserID      string
	Email       string
	PlanType    string
}

// Create runs the full agent identity registration and returns auth.json content.
func Create(opt Options) (*AuthJSON, error) {
	at := strings.TrimSpace(opt.AccessToken)
	if at == "" {
		return nil, fmt.Errorf("empty access_token")
	}

	sess, err := DecodeSession(at)
	if err != nil {
		return nil, err
	}
	if sess.AccountID == "" || sess.UserID == "" {
		return nil, fmt.Errorf("JWT missing account_id/user_id")
	}

	privB64, pubSSH, err := GenerateEd25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("keypair: %w", err)
	}

	client, err := httpx.New(opt.Proxy)
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}
	defer client.Close()

	runtimeID, err := RegisterAgent(client, at, pubSSH)
	if err != nil {
		return nil, err
	}

	if opt.VerifyTask {
		if _, err := RegisterTask(client, at, runtimeID, privB64); err != nil {
			// Non-fatal: auth.json is still usable by Codex CLI.
			_ = err
		}
	}

	return &AuthJSON{
		AuthMode: "agent_identity",
		AgentIdentity: AgentIdentity{
			AgentRuntimeID:          runtimeID,
			AgentPrivateKey:         privB64,
			AccountID:               sess.AccountID,
			ChatGPTUserID:           sess.UserID,
			Email:                   sess.Email,
			PlanType:                sess.PlanType,
			ChatGPTAccountIsFedramp: false,
		},
	}, nil
}

// DecodeSession extracts account fields from a ChatGPT session JWT (no verify).
func DecodeSession(accessToken string) (*SessionInfo, error) {
	claims, err := decodeJWTClaims(accessToken)
	if err != nil {
		return nil, err
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
	}
	if profile == nil {
		profile = map[string]any{}
	}
	plan := str(auth["chatgpt_plan_type"])
	if plan == "" {
		plan = "free"
	}
	return &SessionInfo{
		AccessToken: accessToken,
		AccountID:   str(auth["chatgpt_account_id"]),
		UserID:      str(auth["chatgpt_user_id"]),
		Email:       str(profile["email"]),
		PlanType:    plan,
	}, nil
}

// GenerateEd25519Keypair returns (private_key_pkcs8_base64, public_key_ssh).
func GenerateEd25519Keypair() (privateKeyB64, publicKeySSH string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	privateKeyB64 = base64.StdEncoding.EncodeToString(pkcs8)

	// SSH wire: string("ssh-ed25519") || string(raw_pub_32)
	const keyType = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(keyType)+4+len(pub))
	blob = appendSSHString(blob, []byte(keyType))
	blob = appendSSHString(blob, []byte(pub))
	publicKeySSH = keyType + " " + base64.StdEncoding.EncodeToString(blob)
	return privateKeyB64, publicKeySSH, nil
}

func appendSSHString(b, s []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
	b = append(b, lenBuf[:]...)
	return append(b, s...)
}

// RegisterAgent POSTs /v1/agent/register and returns agent_runtime_id.
func RegisterAgent(client *httpx.Client, accessToken, publicKeySSH string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"abom": map[string]string{
			"agent_version":    AgentVersion,
			"agent_harness_id": AgentHarnessID,
			"running_location": RunningLocation,
		},
		"agent_public_key": publicKeySSH,
	})
	resp, err := client.PostJSON(AuthAPIBase+"/v1/agent/register", body, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"user-agent":    httpx.UserAgent,
	}, false)
	if err != nil {
		return "", fmt.Errorf("agent register: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("agent register HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 400))
	}
	var data map[string]any
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return "", fmt.Errorf("agent register json: %w", err)
	}
	id := str(data["agent_runtime_id"])
	if id == "" {
		return "", fmt.Errorf("agent register: no agent_runtime_id in %s", httpx.DumpSnippet(resp.Body, 300))
	}
	return id, nil
}

// RegisterTask verifies the keypair via signed task registration.
// Returns encrypted_task_id (may be empty).
func RegisterTask(client *httpx.Client, accessToken, agentRuntimeID, privateKeyPKCS8B64 string) (string, error) {
	pkcs8, err := base64.StdEncoding.DecodeString(privateKeyPKCS8B64)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(pkcs8)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	priv, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not ed25519")
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	payload := agentRuntimeID + ":" + ts
	sig := ed25519.Sign(priv, []byte(payload))
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	body, _ := json.Marshal(map[string]string{
		"timestamp": ts,
		"signature": sigB64,
	})
	url := fmt.Sprintf("%s/v1/agent/%s/task/register", AuthAPIBase, agentRuntimeID)
	resp, err := client.PostJSON(url, body, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"user-agent":    httpx.UserAgent,
	}, false)
	if err != nil {
		return "", fmt.Errorf("task register: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("task register HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 400))
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	return str(data["encrypted_task_id"]), nil
}

// WriteAuthJSON writes auth.json with indent.
func WriteAuthJSON(path string, auth *AuthJSON) error {
	if auth == nil {
		return fmt.Errorf("nil auth")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// AppendAuthJSONL appends one compact auth line to a JSONL store.
func AppendAuthJSONL(path string, auth *AuthJSON) error {
	if auth == nil {
		return fmt.Errorf("nil auth")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// SafeFilename turns an email into a filesystem-safe base name.
func SafeFilename(email string) string {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "unknown"
	}
	r := strings.NewReplacer("@", "_at_", "/", "_", "\\", "_", ":", "_", " ", "_")
	s := r.Replace(email)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func decodeJWTClaims(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	payload := parts[1]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(padB64(payload))
		if err != nil {
			return nil, fmt.Errorf("jwt payload decode: %w", err)
		}
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("jwt payload json: %w", err)
	}
	return data, nil
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

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
