package workspace

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func makeJWT(auth map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": auth,
	})
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".sig"
}

func TestJWTIsWorkspaceScoped(t *testing.T) {
	k12 := makeJWT(map[string]any{
		"chatgpt_plan_type":  "k12",
		"chatgpt_account_id": "f638aded-8c12-4035-b1ed-22175741c07f",
	})
	free := makeJWT(map[string]any{
		"user_id": "user-abc",
	})
	if !JWTIsWorkspaceScoped(k12) {
		t.Fatal("expected k12 JWT to be workspace-scoped")
	}
	if JWTPlanType(k12) != "k12" {
		t.Fatalf("plan=%s", JWTPlanType(k12))
	}
	if JWTIsWorkspaceScoped(free) {
		t.Fatal("expected free JWT not workspace-scoped")
	}
	if JWTPlanType(free) != "" {
		t.Fatalf("empty claim should not default, got %q", JWTPlanType(free))
	}
	if JWTIsWorkspaceScoped("") {
		t.Fatal("empty token")
	}
}
