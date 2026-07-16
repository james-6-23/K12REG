package sentinel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k12reg/internal/httpx"
)

const (
	SDKVersion = "20260219f9f6"
	SDKURL     = "https://sentinel.openai.com/sentinel/" + SDKVersion + "/sdk.js"
	ReqURL     = "https://sentinel.openai.com/backend-api/sentinel/req"
	FrameReferer = "https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=" + SDKVersion
	MaxAttempts  = 500_000
	ErrorPrefix  = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"
)

// Bundle is the pair of headers used by auth APIs.
type Bundle struct {
	Token   string // openai-sentinel-token
	SOToken string // openai-sentinel-so-token (optional)
	OaiSC   string // oai-sc cookie value
}

// Generator implements the 2026 Sentinel PoW + dx pipeline.
type Generator struct {
	DeviceID string
	UA       string
	SID      string
	Seed     string
	VMDir    string // dir with run_sentinel_vm.js
}

func NewGenerator(deviceID, ua, vmDir string) *Generator {
	if ua == "" {
		ua = httpx.UserAgent
	}
	return &Generator{
		DeviceID: deviceID,
		UA:       ua,
		SID:      randomUUID(),
		Seed:     fmt.Sprintf("%v", mrand.Float64()),
		VMDir:    vmDir,
	}
}

func fnv1a32(text string) string {
	h := uint32(2166136261)
	for i := 0; i < len(text); i++ {
		h ^= uint32(text[i])
		h = (h * 16777619) & 0xFFFFFFFF
	}
	h ^= h >> 16
	h = (h * 2246822507) & 0xFFFFFFFF
	h ^= h >> 13
	h = (h * 3266489909) & 0xFFFFFFFF
	h ^= h >> 16
	return fmt.Sprintf("%08x", h&0xFFFFFFFF)
}

func (g *Generator) getConfig() []any {
	now := time.Now().UTC()
	dateStr := now.Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
	// Go's format uses different weekday/month abbreviations — match Python strftime closely enough.
	dateStr = now.Format("Mon Jan 2 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	perfNow := 1000 + mrand.Float64()*49000
	navProps := []string{"vendorSub", "productSub", "vendor", "maxTouchPoints", "plugins", "hardwareConcurrency", "cookieEnabled"}
	// U+2212 minus
	navVal := navProps[mrand.Intn(len(navProps))] + "\u2212undefined"
	return []any{
		1920 + 1080,
		dateStr,
		4294705152,
		mrand.Float64(),
		g.UA,
		SDKURL,
		nil,
		"en-US",
		"en-US,en",
		mrand.Float64(),
		navVal,
		[]string{"location", "implementation", "URL"}[mrand.Intn(3)],
		[]string{"Object", "Function", "Array", "Number"}[mrand.Intn(4)],
		perfNow,
		g.SID,
		"",
		[]int{4, 8, 12, 16}[mrand.Intn(4)],
		float64(time.Now().UnixMilli()) - perfNow,
		0, 0, 0, 0, 0, 0, 0,
	}
}

func b64JSON(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func (g *Generator) runCheck(start time.Time, seed, difficulty string, config []any, nonce int) string {
	config[3] = nonce
	config[9] = int(math.Round(time.Since(start).Seconds() * 1000))
	data := b64JSON(config)
	hashHex := fnv1a32(seed + data)
	if len(difficulty) <= len(hashHex) && hashHex[:len(difficulty)] <= difficulty {
		return data + "~S"
	}
	return ""
}

func (g *Generator) requirementsToken() string {
	start := time.Now()
	config := g.getConfig()
	for i := 0; i < MaxAttempts; i++ {
		if r := g.runCheck(start, g.Seed, "0", config, i); r != "" {
			return "gAAAAAC" + r
		}
	}
	return "gAAAAAC" + ErrorPrefix + b64JSON(nil)
}

func (g *Generator) enforcementToken(seed, difficulty string) string {
	start := time.Now()
	config := g.getConfig()
	if difficulty == "" {
		difficulty = "0"
	}
	for i := 0; i < MaxAttempts; i++ {
		if r := g.runCheck(start, seed, difficulty, config, i); r != "" {
			return "gAAAAAB" + r
		}
	}
	return "gAAAAAB" + ErrorPrefix + b64JSON(nil)
}

// Build posts to /sentinel/req and returns header values.
func Build(client *httpx.Client, deviceID, flow, vmDir string) (Bundle, error) {
	g := NewGenerator(deviceID, client.UA, vmDir)
	reqP := g.requirementsToken()

	body, _ := json.Marshal(map[string]string{
		"p":    reqP,
		"id":   deviceID,
		"flow": flow,
	})
	headers := map[string]string{
		"Content-Type":       "text/plain;charset=UTF-8",
		"Referer":            FrameReferer,
		"Origin":             "https://sentinel.openai.com",
		"User-Agent":         g.UA,
		"sec-ch-ua":          httpx.SecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Accept":             "*/*",
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
	}
	resp, err := client.Post(ReqURL, body, headers, false)
	if err != nil {
		return Bundle{}, fmt.Errorf("sentinel req: %w", err)
	}
	var data map[string]any
	_ = json.Unmarshal(resp.Body, &data)
	token, _ := data["token"].(string)
	token = strings.TrimSpace(token)
	if resp.StatusCode != 200 || token == "" {
		return Bundle{}, fmt.Errorf("sentinel_req_failed status=%d body=%s", resp.StatusCode, httpx.DumpSnippet(resp.Body, 200))
	}

	pValue := reqP
	if pow, ok := data["proofofwork"].(map[string]any); ok {
		if reqd, _ := pow["required"].(bool); reqd {
			seed, _ := pow["seed"].(string)
			diff, _ := pow["difficulty"].(string)
			if seed != "" {
				pValue = g.enforcementToken(seed, diff)
			}
		}
	}

	tValue := ""
	if ts, ok := data["turnstile"].(map[string]any); ok {
		if dx, _ := ts["dx"].(string); strings.TrimSpace(dx) != "" {
			tValue = solveDX(strings.TrimSpace(dx), reqP, g.UA, vmDir)
			if tValue == "" {
				// one retry — Node VM can flake under concurrent load
				tValue = solveDX(strings.TrimSpace(dx), reqP, g.UA, vmDir)
			}
			if tValue == "" {
				return Bundle{}, fmt.Errorf("turnstile.dx present but solve_dx empty (flow=%s vm=%s)", flow, vmDir)
			}
		}
	}

	soValue := ""
	if so, ok := data["so"].(map[string]any); ok {
		dx, _ := so["snapshot_dx"].(string)
		if dx == "" {
			dx, _ = so["dx"].(string)
		}
		if strings.TrimSpace(dx) != "" {
			soValue = solveDX(strings.TrimSpace(dx), reqP, g.UA, vmDir)
			if soValue == "" {
				soValue = solveDX(strings.TrimSpace(dx), reqP, g.UA, vmDir)
			}
		}
	}

	sentinelObj := map[string]string{
		"p":    pValue,
		"t":    tValue,
		"c":    token,
		"id":   deviceID,
		"flow": flow,
	}
	tokBytes, err := jsonCompact(sentinelObj)
	if err != nil {
		return Bundle{}, err
	}

	oaiSC := "0" + token
	// Match Python: set oai-sc on auth + parent openai + sentinel domains
	for _, dom := range []string{"auth.openai.com", ".auth.openai.com", ".openai.com", "sentinel.openai.com"} {
		client.SetCookie("oai-sc", oaiSC, dom)
	}

	b := Bundle{Token: string(tokBytes), OaiSC: oaiSC}
	if soValue != "" {
		soObj := map[string]string{
			"so":   soValue,
			"c":    token,
			"id":   deviceID,
			"flow": flow,
		}
		soBytes, err := jsonCompact(soObj)
		if err != nil {
			return Bundle{}, err
		}
		b.SOToken = string(soBytes)
	}
	return b, nil
}

// jsonCompact encodes without HTML escaping (critical for sentinel tokens).
func jsonCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

func solveDX(dx, secret, ua, vmDir string) string {
	if vmDir == "" {
		// try common relative paths from repo root
		for _, cand := range []string{
			"scripts/sentinel_vm",
			filepath.Join("scripts", "sentinel_vm"),
			"../scripts/sentinel_vm",
		} {
			if st, err := os.Stat(filepath.Join(cand, "run_sentinel_vm.js")); err == nil && !st.IsDir() {
				vmDir = cand
				break
			}
		}
	}
	if vmDir == "" {
		return ""
	}
	absVM, err := filepath.Abs(vmDir)
	if err == nil {
		vmDir = absVM
	}
	runner := filepath.Join(vmDir, "run_sentinel_vm.js")
	if st, err := os.Stat(runner); err != nil || st.IsDir() {
		return ""
	}
	// sdkvm.js must sit next to the runner
	if st, err := os.Stat(filepath.Join(vmDir, "sdkvm.js")); err != nil || st.IsDir() {
		return ""
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return ""
	}
	payload, _ := jsonCompact(map[string]any{
		"secret":         secret,
		"encodedPayload": dx,
		"userAgent":      ua,
		"locationHref":   "https://auth.openai.com/",
		"timeoutMs":      4000,
	})
	// Hard cap Node process so a hung VM never freezes the worker indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, runner)
	cmd.Dir = vmDir
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ""
	}
	var out struct {
		OK     bool `json:"ok"`
		Error  string `json:"error"`
		Result struct {
			Channel      string `json:"channel"`
			EncodedValue string `json:"encodedValue"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || !out.OK {
		return ""
	}
	if out.Result.Channel == "resolve" && out.Result.EncodedValue != "" {
		return out.Result.EncodedValue
	}
	return ""
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
