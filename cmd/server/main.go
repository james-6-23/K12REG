package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k12reg/internal/codexagent"
	"k12reg/internal/config"
	"k12reg/internal/mail"
	"k12reg/internal/pipeline"
	"k12reg/internal/storage"
	"k12reg/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
			runServe()
			return
		case "codex-agent", "codex_agent":
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
			runCodexAgent()
			return
		}
	}
	runCLI()
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address (default :$PORT or :8000)")
	dataDir := fs.String("data", envOr("K12_DATA_DIR", "."), "data directory")
	password := fs.String("password", envOr("WEB_PASSWORD", "admin"), "web login password")
	staticDir := fs.String("static", envOr("K12_STATIC_DIR", ""), "SPA static dir (default: <exe>/static or ./static)")
	sessionDays := fs.Int("session-days", envInt("WEB_SESSION_DAYS", 30), "signed cookie days")
	_ = fs.Parse(os.Args[1:])

	if *addr == "" {
		port := envOr("PORT", "8000")
		*addr = ":" + port
	}
	dataAbs, _ := filepath.Abs(*dataDir)
	static := *staticDir
	if static == "" {
		static = findStatic()
	}

	srv := web.New(web.Options{
		DataDir:     dataAbs,
		Password:    *password,
		StaticDir:   static,
		SessionDays: *sessionDays,
	})

	fmt.Printf("K12REG Go control panel\n")
	fmt.Printf("  listen   %s\n", *addr)
	fmt.Printf("  data     %s\n", dataAbs)
	fmt.Printf("  static   %s\n", static)
	fmt.Printf("  password (env WEB_PASSWORD)\n")
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

func runCLI() {
	var (
		dataDir    = flag.String("data", envOr("K12_DATA_DIR", "."), "data directory")
		settings   = flag.String("settings", "", "settings.json path")
		count      = flag.Int("count", 0, "accounts to register")
		threads    = flag.Int("threads", 0, "concurrency")
		proxy      = flag.String("proxy", "", "single proxy URL")
		mailboxes  = flag.String("mailboxes", "", "mailbox pool file")
		sentinelVM = flag.String("sentinel-vm", "", "sentinel VM dir")
		noDomain   = flag.Bool("no-domain-gate", false, "disable same-domain gate")
		proxyTry   = flag.Int("proxy-retries", 0, "proxy rotations on soft register fail")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  k12reg serve [flags]          # Go web control panel\n")
		fmt.Fprintf(os.Stderr, "  k12reg codex-agent [flags]    # Codex Agent Identity from AT\n")
		fmt.Fprintf(os.Stderr, "  k12reg [flags]                # CLI pipeline\n\n")
		fmt.Fprintf(os.Stderr, "Serve flags: k12reg serve -h\n")
		fmt.Fprintf(os.Stderr, "Codex agent: k12reg codex-agent -h\nCLI flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg := config.Default()
	cfg.DataDir = *dataDir

	settingsPath := *settings
	if settingsPath == "" {
		cand := filepath.Join(cfg.DataDir, "settings.json")
		if _, err := os.Stat(cand); err == nil {
			settingsPath = cand
		}
	}
	if settingsPath != "" {
		loaded, err := config.LoadJSON(settingsPath)
		if err != nil {
			log.Fatalf("load settings: %v", err)
		}
		loaded.DataDir = cfg.DataDir
		cfg = loaded
		fmt.Printf("loaded settings %s\n", settingsPath)
	}

	if *count > 0 {
		cfg.Total = *count
	}
	if *threads > 0 {
		cfg.Threads = *threads
	}
	if *proxy != "" {
		cfg.Proxy = *proxy
	}
	if *mailboxes != "" {
		cfg.MailboxesFile = *mailboxes
	}
	if *noDomain {
		cfg.RequireSameDomain = false
	}
	if *proxyTry > 0 {
		cfg.RegisterProxyRetries = *proxyTry
	}

	cfg.SentinelVMDir = *sentinelVM
	if cfg.SentinelVMDir == "" {
		cfg.SentinelVMDir = os.Getenv("K12_SENTINEL_VM")
	}
	if cfg.SentinelVMDir == "" {
		cfg.SentinelVMDir = findSentinelVM()
	}
	if cfg.SentinelVMDir != "" {
		fmt.Printf("sentinel VM: %s\n", cfg.SentinelVMDir)
	}

	mailFile := cfg.ResolvePath(cfg.MailboxesFile)
	pool, err := mail.LoadPool(mailFile, cfg.ResolvePath("outlook_token_state.json"), cfg.AliasCount)
	if err != nil {
		log.Fatalf("mail pool: %v", err)
	}
	if n := pool.SeedUsed(storage.LoadRegisteredEmails(cfg.DataDir)); n > 0 {
		fmt.Printf("mail pool: seeded %d used from registered_accounts (available=%d)\n", n, pool.Available())
	}
	fmt.Printf("mail pool: %s\n", mailFile)

	var proxies []string
	if cfg.Proxy == "" && cfg.ProxiesFile != "" {
		pp := cfg.ResolvePath(cfg.ProxiesFile)
		proxies, err = config.LoadProxies(pp, cfg.DefaultProtocol)
		if err != nil {
			fmt.Printf("warn: proxies: %v\n", err)
		} else {
			fmt.Printf("proxies: %d from %s\n", len(proxies), pp)
		}
	} else if cfg.Proxy != "" {
		proxies = []string{cfg.Proxy}
	}

	fmt.Printf("start · total=%d threads=%d workspace=%v oauth=%s\n",
		cfg.Total, cfg.Threads, cfg.WorkspaceEnabled, cfg.OAuthPath)
	st, err := pipeline.Run(pipeline.Options{
		Config:  cfg,
		Proxies: proxies,
		Pool:    pool,
		Log:     func(s string) { fmt.Println(s) },
	})
	if err != nil {
		log.Fatalf("pipeline: %v", err)
	}
	fmt.Println("── summary ──")
	fmt.Printf("registered : %d\n", st.Registered)
	fmt.Printf("join ok    : %d\n", st.JoinOK)
	fmt.Printf("approve ok : %d\n", st.ApproveOK)
	fmt.Printf("k12 plan   : %d\n", st.K12)
	fmt.Printf("import ok  : %d\n", st.ImportOK)
	fmt.Printf("fail       : %d\n", st.Fail)
	fmt.Printf("elapsed    : %s\n", st.Elapsed.Round(time.Second))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		return def
	}
	return n
}

func findStatic() string {
	candidates := []string{
		"frontend/dist",
		filepath.Join("frontend", "dist"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append([]string{
			filepath.Join(dir, "frontend", "dist"),
			filepath.Join(dir, "static"),
		}, candidates...)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "frontend", "dist"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return filepath.Join("frontend", "dist")
}

// runCodexAgent registers Codex CLI agent_identity auth.json from access tokens.
// Port of Tool/codex-agent/codex_agent.py.
func runCodexAgent() {
	fs := flag.NewFlagSet("codex-agent", flag.ExitOnError)
	dataDir := fs.String("data", envOr("K12_DATA_DIR", "."), "data directory")
	token := fs.String("token", "", "ChatGPT session JWT (accessToken)")
	file := fs.String("file", "", "JSON with accessToken, or plain access_token.txt (one JWT per line)")
	output := fs.String("output", "", "single-token output path (default: <data>/codex_auth/<email>.json)")
	fromAccounts := fs.Bool("from-accounts", false, "use access_token from registered_accounts.jsonl")
	fromTokenFile := fs.Bool("from-access-token-file", false, "read data/access_token.txt")
	proxy := fs.String("proxy", "", "proxy URL")
	noVerify := fs.Bool("no-verify", false, "skip task registration verify")
	outDir := fs.String("out-dir", "codex_auth", "output directory under data (batch mode)")
	_ = fs.Parse(os.Args[1:])

	dataAbs, _ := filepath.Abs(*dataDir)
	var tokens []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" || strings.HasPrefix(t, "#") {
			return
		}
		tokens = append(tokens, t)
	}

	if *token != "" {
		add(*token)
	}
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			log.Fatalf("read file: %v", err)
		}
		// Try JSON first
		var data map[string]any
		if json.Unmarshal(b, &data) == nil {
			if v, _ := data["accessToken"].(string); v != "" {
				add(v)
			} else if v, _ := data["access_token"].(string); v != "" {
				add(v)
			} else {
				log.Fatalf("JSON file has no accessToken / access_token")
			}
		} else {
			sc := bufio.NewScanner(strings.NewReader(string(b)))
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				add(sc.Text())
			}
		}
	}
	if *fromTokenFile {
		p := filepath.Join(dataAbs, "access_token.txt")
		f, err := os.Open(p)
		if err != nil {
			log.Fatalf("open access_token.txt: %v", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			add(sc.Text())
		}
		_ = f.Close()
	}
	if *fromAccounts {
		path := storage.AccountsFile(dataAbs)
		f, err := os.Open(path)
		if err != nil {
			log.Fatalf("open accounts: %v", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var raw map[string]any
			if json.Unmarshal(line, &raw) != nil {
				continue
			}
			if v, _ := raw["access_token"].(string); v != "" {
				add(v)
			}
		}
		_ = f.Close()
	}

	// Interactive if nothing provided
	if len(tokens) == 0 {
		fmt.Println("请输入 ChatGPT session JWT (accessToken)：")
		fmt.Println("（或使用 --token / --file / --from-access-token-file / --from-accounts）")
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			add(sc.Text())
		}
	}
	if len(tokens) == 0 {
		log.Fatal("未提供 access_token")
	}

	// Dedupe
	seen := map[string]struct{}{}
	var unique []string
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		unique = append(unique, t)
	}
	tokens = unique

	dir := filepath.Join(dataAbs, filepath.Base(*outDir))
	if *outDir == "" {
		dir = filepath.Join(dataAbs, "codex_auth")
	}
	verify := !*noVerify
	okN, failN := 0, 0

	fmt.Printf("codex-agent · tokens=%d · out=%s · verify=%v\n", len(tokens), dir, verify)

	for i, tok := range tokens {
		auth, err := codexagent.Create(codexagent.Options{
			AccessToken: tok,
			Proxy:       *proxy,
			VerifyTask:  verify,
		})
		if err != nil {
			fmt.Printf("[%d/%d] FAIL · %v\n", i+1, len(tokens), err)
			failN++
			continue
		}
		base := codexagent.SafeFilename(auth.AgentIdentity.Email)
		if base == "unknown" {
			base = codexagent.SafeFilename(auth.AgentIdentity.AccountID)
		}
		path := *output
		if path == "" || len(tokens) > 1 {
			path = filepath.Join(dir, base+".json")
		}
		if err := codexagent.WriteAuthJSON(path, auth); err != nil {
			fmt.Printf("[%d/%d] FAIL write · %v\n", i+1, len(tokens), err)
			failN++
			continue
		}
		_ = codexagent.AppendAuthJSONL(filepath.Join(dir, "agents.jsonl"), auth)
		fmt.Printf("[%d/%d] OK · %s · runtime=%s · %s\n",
			i+1, len(tokens), auth.AgentIdentity.Email, auth.AgentIdentity.AgentRuntimeID, path)
		okN++
	}
	fmt.Printf("── summary · ok=%d fail=%d ──\n", okN, failN)
	if failN > 0 && okN == 0 {
		os.Exit(1)
	}
}

func findSentinelVM() string {
	candidates := []string{
		filepath.Join("scripts", "sentinel_vm"),
		"scripts/sentinel_vm",
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 6; i++ {
			candidates = append(candidates, filepath.Join(dir, "scripts", "sentinel_vm"))
			p := filepath.Dir(dir)
			if p == dir {
				break
			}
			dir = p
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "run_sentinel_vm.js")); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}
