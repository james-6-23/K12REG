package workspace

import (
	"fmt"
	"testing"
)

func TestIsTokenInvalidated(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`models HTTP 401: {"error":{"code":"token_invalidated","message":"Your authentication token has been invalidated."}}`, true},
		{`accounts/check HTTP 401: token has been revoked`, true},
		{`accounts/check HTTP 500: internal`, false},
		{`connection reset by peer`, false},
	}
	for _, tc := range cases {
		got := IsTokenInvalidated(fmt.Errorf("%s", tc.msg))
		if got != tc.want {
			t.Errorf("IsTokenInvalidated(%q) = %v want %v", tc.msg[:min(50, len(tc.msg))], got, tc.want)
		}
	}
	if IsTokenInvalidated(nil) {
		t.Fatal("nil should not be invalidated")
	}
}

func TestProbeAccessTokenEmpty(t *testing.T) {
	if err := ProbeAccessToken("", "", ""); err == nil {
		t.Fatal("expected error for empty token")
	}
}
