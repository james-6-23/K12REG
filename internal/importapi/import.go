package importapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"k12reg/internal/httpx"
)

const EndpointPath = "/api/admin/accounts/at"

// Result of one import POST.
type Result struct {
	OK      bool
	Status  int
	Outcome string // added | updated | duplicate | failed | unknown
	Message string
	Error   string
}

// Push posts access_token to {baseURL}/api/admin/accounts/at.
func Push(baseURL, adminKey, accessToken, proxy string) Result {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || accessToken == "" {
		return Result{OK: false, Outcome: "failed", Error: "missing url or token"}
	}
	client, err := httpx.New(proxy)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	defer client.Close()

	u := baseURL + EndpointPath
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	resp, err := client.PostJSON(u, body, map[string]string{
		"X-Admin-Key": adminKey,
		"Accept":      "application/json",
	}, false)
	if err != nil {
		return Result{OK: false, Outcome: "failed", Error: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{
			OK: false, Status: resp.StatusCode, Outcome: "failed",
			Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 160)),
		}
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	msg, _ := data["message"].(string)
	outcome := classify(data)
	return Result{OK: outcome != "failed", Status: resp.StatusCode, Outcome: outcome, Message: msg}
}

func classify(data map[string]any) string {
	if data == nil {
		return "unknown"
	}
	n := func(k string) int {
		switch t := data[k].(type) {
		case float64:
			return int(t)
		case int:
			return t
		default:
			return 0
		}
	}
	switch {
	case n("success") > 0 || n("added") > 0:
		return "added"
	case n("updated") > 0:
		return "updated"
	case n("duplicate") > 0:
		return "duplicate"
	case n("failed") > 0:
		return "failed"
	default:
		return "unknown"
	}
}
