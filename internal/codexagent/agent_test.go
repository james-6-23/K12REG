package codexagent

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateEd25519Keypair_RoundTrip(t *testing.T) {
	privB64, pubSSH, err := GenerateEd25519Keypair()
	if err != nil {
		t.Fatal(err)
	}
	if privB64 == "" || !strings.HasPrefix(pubSSH, "ssh-ed25519 ") {
		t.Fatalf("bad keypair: priv=%q pub=%q", privB64[:min(20, len(privB64))], pubSSH)
	}

	raw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		t.Fatal(err)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	priv, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("not ed25519")
	}
	msg := []byte("agent-id:2026-01-01T00:00:00Z")
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), msg, sig) {
		t.Fatal("sign/verify failed")
	}
}

func TestDecodeSession(t *testing.T) {
	// Minimal unsigned JWT with OpenAI claim namespaces.
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-123",
			"chatgpt_user_id":    "user-abc",
			"chatgpt_plan_type":  "k12",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "test@example.com",
		},
	}
	b, _ := json.Marshal(payload)
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(b) + ".sig"

	sess, err := DecodeSession(jwt)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccountID != "acc-123" || sess.UserID != "user-abc" {
		t.Fatalf("got %+v", sess)
	}
	if sess.Email != "test@example.com" || sess.PlanType != "k12" {
		t.Fatalf("got %+v", sess)
	}
}

func TestSafeFilename(t *testing.T) {
	if got := SafeFilename("Foo@Bar.com"); got != "foo_at_bar.com" {
		t.Fatalf("got %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
