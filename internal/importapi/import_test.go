package importapi

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"":               ModeAT,
		"at":             ModeAT,
		"access_token":   ModeAT,
		"agent_identity": ModeAgentIdentity,
		"agent-identity": ModeAgentIdentity,
		"Agent":          ModeAgentIdentity,
	}
	for in, want := range cases {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAuthJSONToString(t *testing.T) {
	s, err := authJSONToString(map[string]any{
		"auth_mode": "agent_identity",
		"agent_identity": map[string]any{
			"agent_runtime_id": "agent-x",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s == "" || s[0] != '{' {
		t.Fatalf("bad: %q", s)
	}
	s2, err := authJSONToString(s)
	if err != nil || s2 != s {
		t.Fatalf("string passthrough: %v %q", err, s2)
	}
}
