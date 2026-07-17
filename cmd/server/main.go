package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"k12reg/internal/config"
	"k12reg/internal/mail"
	"k12reg/internal/pipeline"
	"k12reg/internal/storage"
	"k12reg/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		runServe()
		return
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
		fmt.Fprintf(os.Stderr, "  k12reg serve [flags]     # Go web control panel\n")
		fmt.Fprintf(os.Stderr, "  k12reg [flags]           # CLI pipeline\n\n")
		fmt.Fprintf(os.Stderr, "Serve flags: k12reg serve -h\nCLI flags:\n")
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
