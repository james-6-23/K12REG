package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k12reg/internal/config"
	"k12reg/internal/importapi"
	"k12reg/internal/mail"
	"k12reg/internal/register"
	"k12reg/internal/storage"
	"k12reg/internal/workspace"
)

type Options struct {
	Config  config.Config
	Proxies []string
	Pool    *mail.Pool
	Log     func(string)
	Ctx     context.Context
}

// Stats summarizes a pipeline run.
type Stats struct {
	Registered int64
	JoinOK     int64
	ApproveOK  int64
	K12        int64
	ImportOK   int64
	Fail       int64
	Elapsed    time.Duration
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, args...))
	}
}

// Run executes N accounts with limited concurrency.
func Run(opt Options) (stats Stats, err error) {
	cfg := opt.Config
	if err := cfg.Validate(); err != nil {
		return stats, err
	}
	if opt.Pool == nil {
		return stats, fmt.Errorf("mail pool is nil")
	}
	ctx := opt.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	start := time.Now()
	defer func() { stats.Elapsed = time.Since(start) }()

	var mgr workspace.ManagerSession
	var hasMgr bool
	if cfg.WorkspaceEnabled && cfg.ApproveRequests {
		path := cfg.ResolvePath(cfg.ManagerSessionFile)
		if m, e := workspace.LoadManagerSession(path); e == nil {
			mgr = m
			hasMgr = true
			opt.log("manager session · account=%s email=%s", trunc(mgr.AccountID, 12), mgr.Email)
		} else {
			opt.log("manager session skip: %v", e)
		}
	}

	// K12 domain gate: only acquire mailboxes on manager domain.
	if cfg.RequireSameDomain && hasMgr && mgr.Email != "" {
		dom := config.EmailDomain(mgr.Email)
		if dom != "" {
			opt.Pool.SetRequireDomain(dom)
			opt.log("domain gate · require @%s · available=%d", dom, opt.Pool.Available())
		}
	}

	workers := cfg.Threads
	if workers < 1 {
		workers = 1
	}
	if workers > cfg.Total {
		workers = cfg.Total
	}

	jobs := make(chan int, cfg.Total)
	for i := 1; i <= cfg.Total; i++ {
		jobs <- i
	}
	close(jobs)

	var (
		wg      sync.WaitGroup
		proxyIx atomic.Int64
	)

	// JSONL append-only store (migrates legacy registered_accounts.json automatically).
	accountsPath := storage.AccountsFile(cfg.DataDir)
	tokenPath := cfg.ResolvePath("access_token.txt")
	proxyRetries := cfg.RegisterProxyRetries
	if proxyRetries < 1 {
		proxyRetries = 1
	}

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			select {
			case <-ctx.Done():
				opt.log("[%d/%d] cancelled", idx, cfg.Total)
				return
			default:
			}
			tag := fmt.Sprintf("[%d/%d]", idx, cfg.Total)
			mb, err := opt.Pool.Acquire()
			if err != nil {
				opt.log("%s acquire mailbox: %v", tag, err)
				atomic.AddInt64(&stats.Fail, 1)
				continue
			}

			// Extra domain check (defense in depth).
			if cfg.RequireSameDomain && hasMgr && mgr.Email != "" &&
				!config.SameEmailDomain(mb.Address, mgr.Email) {
				opt.Pool.Release(mb)
				opt.log("%s skip · domain mismatch %s vs manager %s",
					tag, config.EmailDomain(mb.Address), config.EmailDomain(mgr.Email))
				atomic.AddInt64(&stats.Fail, 1)
				continue
			}

			if ctx.Err() != nil {
				opt.Pool.Release(mb)
				opt.log("%s cancelled", tag)
				return
			}
			reg, proxy, regErr := registerWithProxyRetry(ctx, opt, cfg, mb, tag, &proxyIx, proxyRetries)
			if regErr != nil {
				if errors.Is(regErr, context.Canceled) || errors.Is(regErr, context.DeadlineExceeded) {
					opt.Pool.Release(mb)
					opt.log("%s cancelled", tag)
					return
				}
				// Bad Graph refresh_token / compromised / already-used stock:
				// mark base + aliases as used so they are not picked again.
				if mail.IsGraphAuthPermanent(regErr) {
					opt.Pool.MarkGraphDead(mb)
					opt.log("%s REGISTER FAIL (graph dead · marked used): %v", tag, regErr)
				} else {
					opt.Pool.Mark(mb, false)
					opt.log("%s REGISTER FAIL: %v", tag, regErr)
				}
				atomic.AddInt64(&stats.Fail, 1)
				continue
			}
			opt.Pool.Mark(mb, true)
			atomic.AddInt64(&stats.Registered, 1)
			opt.log("%s registered · AT=%v RT=%v · proxy=%s",
				tag, reg.AccessToken != "", reg.RefreshToken != "", maskProxy(proxy))

			acc := storage.Account{
				"email":         reg.Email,
				"password":      reg.Password,
				"access_token":  reg.AccessToken,
				"refresh_token": reg.RefreshToken,
				"id_token":      reg.IDToken,
				"source_type":   reg.SourceType,
				"created_at":    reg.CreatedAt,
			}

			// Join only the currently selected workspace (not every id in the pool).
			activeWID := cfg.ActiveWorkspaceID()
			if cfg.WorkspaceEnabled && activeWID != "" {
				var joins []workspace.JoinResult
				opt.log("%s join workspace %s …", tag, trunc(activeWID, 12))
				jr := workspace.Join(reg.AccessToken, activeWID, cfg.WorkspaceRoute, proxy)
				joins = append(joins, jr)
				if jr.OK {
					opt.log("%s join ok · %s", tag, trunc(activeWID, 12))
				} else {
					opt.log("%s join fail · %s · %s", tag, trunc(activeWID, 12), jr.Error)
				}
				acc["workspace_id"] = activeWID
				acc["join_results"] = joins
				acc["join_status"] = joinStatus(joins)
				if joinStatus(joins) == "ok" {
					atomic.AddInt64(&stats.JoinOK, 1)
				}

				if hasMgr && cfg.ApproveRequests && joinStatus(joins) == "ok" {
					opt.log("%s approve · %s …", tag, reg.Email)
					if err := workspace.ApproveByEmail(mgr, reg.Email, proxy, cfg.ApproveMaxAttempts); err != nil {
						opt.log("%s approve fail: %v", tag, err)
						acc["approve_status"] = "failed"
					} else {
						opt.log("%s approve ok", tag)
						acc["approve_status"] = "ok"
						atomic.AddInt64(&stats.ApproveOK, 1)
						// Brief wait for membership propagation (interruptible).
						if err := sleepCtx(ctx, 1500*time.Millisecond); err != nil {
							return
						}
					}
				} else if hasMgr && cfg.ApproveRequests {
					acc["approve_status"] = "skipped"
				}

				// Refresh AT then re-check plan (membership often needs fresh token).
				at := reg.AccessToken
				if reg.RefreshToken != "" && acc["approve_status"] == "ok" {
					if newAT, e := workspace.RefreshAccessToken(reg.RefreshToken, proxy); e == nil && newAT != "" {
						at = newAT
						reg.AccessToken = newAT
						acc["access_token"] = newAT
						opt.log("%s token refresh ok", tag)
					} else if e != nil {
						opt.log("%s token refresh: %v", tag, e)
					}
				}

				if plan, aid, err := workspace.CheckPlan(at, proxy, cfg.ActiveWorkspaceIDs()); err == nil {
					if plan != "" {
						acc["plan_type"] = plan
					}
					if aid != "" {
						acc["chatgpt_account_id"] = aid
					}
					opt.log("%s plan=%s account=%s", tag, plan, trunc(aid, 12))
					if isK12(plan) {
						atomic.AddInt64(&stats.K12, 1)
					}
				} else {
					opt.log("%s plan check: %v", tag, err)
				}
			}

			// Optional import to one or more account pools.
			importEps := cfg.ActiveImportEndpoints()
			if len(importEps) > 0 && reg.AccessToken != "" {
				plan := strings.ToLower(str(acc["plan_type"]))
				var results []map[string]any
				okN, failN, skipN := 0, 0, 0
				for _, ep := range importEps {
					label := ep.Name
					if label == "" {
						label = ep.URL
					}
					reqK12 := ep.RequireK12
					if reqK12 && !isK12(plan) {
						skipN++
						results = append(results, map[string]any{
							"name": label, "url": ep.URL, "status": "skipped", "reason": "plan=" + plan,
						})
						opt.log("%s import skip · %s · plan=%s", tag, label, plan)
						continue
					}
					// Import to own account-pool APIs should go direct (no reg proxy).
					// Residential SOCKS often RST/EOF when tunneling to arbitrary hosts.
					ir := importapi.Push(ep.URL, ep.AdminKey, reg.AccessToken, "")
					if !ir.OK && isImportNetErr(ir.Error) {
						// One retry after brief backoff (API/proxy blips).
						if err := sleepCtx(ctx, 800*time.Millisecond); err != nil {
							return
						}
						ir = importapi.Push(ep.URL, ep.AdminKey, reg.AccessToken, "")
					}
					entry := map[string]any{
						"name": label, "url": ep.URL, "status": ir.Outcome, "ok": ir.OK,
					}
					if ir.Error != "" {
						entry["error"] = ir.Error
					}
					results = append(results, entry)
					if ir.OK {
						okN++
						opt.log("%s import ok · %s · %s", tag, label, ir.Outcome)
					} else {
						failN++
						opt.log("%s import fail · %s · %s", tag, label, ir.Error)
					}
				}
				acc["import_results"] = results
				switch {
				case okN > 0 && failN == 0 && skipN == 0:
					acc["import_status"] = "ok"
					atomic.AddInt64(&stats.ImportOK, 1)
				case okN > 0 && (failN > 0 || skipN > 0):
					acc["import_status"] = fmt.Sprintf("partial %d/%d", okN, len(importEps))
					atomic.AddInt64(&stats.ImportOK, 1)
				case skipN == len(importEps):
					acc["import_status"] = "skipped"
				case failN > 0:
					acc["import_status"] = "failed"
				default:
					acc["import_status"] = "unknown"
				}
			}

			if err := storage.AppendAccount(accountsPath, acc); err != nil {
				opt.log("%s save accounts: %v", tag, err)
			}
			// Prefer writing token when plan is k12 (or always if no workspace).
			writeTok := reg.AccessToken
			if cfg.WorkspaceEnabled && cfg.ImportRequireK12 {
				if !isK12(str(acc["plan_type"])) {
					writeTok = "" // still saved in JSON; skip free AT line
				}
			}
			if writeTok != "" {
				if err := storage.AppendAccessToken(tokenPath, writeTok); err != nil {
					opt.log("%s save token: %v", tag, err)
				}
			}
			opt.log("%s DONE", tag)
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}
	wg.Wait()
	// Surface cancel so RunManager can mark exit=130 instead of a normal summary.
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

func registerWithProxyRetry(
	ctx context.Context,
	opt Options,
	cfg config.Config,
	mb mail.Mailbox,
	tag string,
	proxyIx *atomic.Int64,
	maxAttempts int,
) (*register.Result, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		proxy := pickProxy(opt, proxyIx)
		if attempt > 1 {
			opt.log("%s register retry %d/%d · proxy=%s", tag, attempt, maxAttempts, maskProxy(proxy))
		} else {
			opt.log("%s start · %s · proxy=%s", tag, mb.Address, maskProxy(proxy))
		}
		reg, err := register.Run(register.Options{
			Proxy:         proxy,
			Mailbox:       mb,
			OTPTimeout:    time.Duration(cfg.WaitTimeout * float64(time.Second)),
			OTPInterval:   time.Duration(cfg.WaitInterval * float64(time.Second)),
			SentinelVMDir: cfg.SentinelVMDir,
			Log:           func(s string) { opt.log("%s %s", tag, s) },
			Ctx:           ctx,
		})
		if err == nil {
			return reg, proxy, nil
		}
		lastErr = err
		// Prefer ctx error when Stop closed the HTTP session mid-request.
		if err := ctx.Err(); err != nil {
			return nil, proxy, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, proxy, err
		}
		if !isRetryableRegister(err) || attempt == maxAttempts {
			return nil, proxy, err
		}
		// Full Run() will re-do authorize + register on next attempt.
		opt.log("%s soft-fail · rotate proxy & re-authorize · %v", tag, err)
	}
	return nil, "", lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func pickProxy(opt Options, proxyIx *atomic.Int64) string {
	if len(opt.Proxies) > 0 {
		i := proxyIx.Add(1) - 1
		return opt.Proxies[int(i)%len(opt.Proxies)]
	}
	return opt.Config.Proxy
}

func isImportNetErr(msg string) bool {
	s := strings.ToLower(msg)
	for _, k := range []string{
		"connection reset", "eof", "timeout", "broken pipe", "connection refused",
		"i/o timeout", "tls", "handshake", "no such host", "network is unreachable",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func isRetryableRegister(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Hard failures: already spent OTP wait or burned auth — do NOT re-run full
	// authorize→OTP (that multiplies wall-clock by RegisterProxyRetries).
	hard := []string{
		"wrong code", "wrong_email_otp", "otp timeout",
		"validate_otp", "graph auth failed", "aadsts70000",
		"compromised", "registration_disallowed", "user_already",
		"domain likely banned", "turnstile.dx", "pool exhausted",
		// Already waited full OTP window — re-running wastes another timeout.
		"otp timeout after",
	}
	for _, k := range hard {
		if strings.Contains(s, k) {
			return false
		}
	}
	// Soft / infra before OTP — rotate proxy and re-run from authorize.
	// Note: plain "timeout" only counts as soft if not an OTP timeout (handled above).
	keys := []string{
		"handshake", "tls", "connection", "eof", "reset", "proxy", "network",
		"i/o timeout", "deadline exceeded",
		"503", "502", "429", "cf_service", "service unavailable", "cloudflare",
		// OpenAI auth state flake (often concurrent / sticky session issues)
		"invalid_auth_step",
		"invalid authorization step",
		// authorize / register step network flake (not OTP)
		"platform_authorize", "user_register", "sentinel req", "sentinel_req",
	}
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	// Generic "timeout" without otp prefix: soft (hung HTTP before OTP).
	if strings.Contains(s, "timeout") {
		return true
	}
	return false
}

func isK12(plan string) bool {
	p := strings.ToLower(strings.TrimSpace(plan))
	return p == "k12" || p == "team" || p == "enterprise" || p == "edu"
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func joinStatus(joins []workspace.JoinResult) string {
	if len(joins) == 0 {
		return "skipped"
	}
	for _, j := range joins {
		if j.OK {
			return "ok"
		}
	}
	return "failed"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func maskProxy(p string) string {
	if p == "" {
		return "(none)"
	}
	if i := strings.Index(p, "@"); i >= 0 {
		head := p
		if i > 12 {
			head = p[:12]
		} else {
			head = p[:i]
		}
		return head + "***" + p[i:]
	}
	if len(p) > 40 {
		return p[:40] + "…"
	}
	return p
}
