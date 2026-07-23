package register

import (
	"net/url"
	"strings"
	"testing"
)

func TestInjectAuthorizePKCE(t *testing.T) {
	challenge := "test-challenge-abc"
	raw := "https://auth.openai.com/api/accounts/authorize?client_id=app_X8z&scope=openid+email&response_type=code&state=xyz"
	out := injectAuthorizePKCE(raw, challenge)
	if out == "" {
		t.Fatal("expected patched URL")
	}
	u, err := url.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge") != challenge {
		t.Fatalf("code_challenge=%q", q.Get("code_challenge"))
	}
	if !strings.Contains(q.Get("scope"), "offline_access") {
		t.Fatalf("scope missing offline_access: %q", q.Get("scope"))
	}
}

func TestResolveAuthorizeURLPrefersNextAuth(t *testing.T) {
	next := "https://auth.openai.com/api/accounts/authorize?client_id=app_X8z&state=s1"
	u, src := resolveAuthorizeURL(next, "a@b.com", "did")
	if src != "nextauth" || u != next {
		t.Fatalf("src=%q url=%q", src, u)
	}
	u2, src2 := resolveAuthorizeURL("", "a@b.com", "did")
	if src2 != "built" {
		t.Fatalf("src=%q want built", src2)
	}
	q, _ := url.Parse(u2)
	if q.Query().Get("code_challenge") != "" {
		t.Fatalf("built fallback must not force PKCE: %s", u2)
	}
	if q.Query().Get("client_id") != ClientID {
		t.Fatalf("client_id=%q", q.Query().Get("client_id"))
	}
}

func TestForcePKCEAuthorizeURLDoesNotInject(t *testing.T) {
	// Even if a challenge is passed, NextAuth URL must be used as-is (no inject).
	next := "https://auth.openai.com/api/accounts/authorize?client_id=app_X8z&state=s1"
	u, src := forcePKCEAuthorizeURL(next, "a@b.com", "did", "ch-1")
	if src != "nextauth" || u != next {
		t.Fatalf("must not inject PKCE into nextauth url: src=%q", src)
	}
	if strings.Contains(u, "code_challenge") {
		t.Fatalf("injected challenge into nextauth url: %s", u)
	}
}
