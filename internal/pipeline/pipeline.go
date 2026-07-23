package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k12reg/internal/codexagent"
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

	oauthPath := register.NormalizeOAuthPath(cfg.OAuthPath)
	opt.log("oauth path · %s", oauthPath)

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
				// Permanent burns (cannot usefully retry this address):
				//  - Graph token dead
				//  - OpenAI identity dead: already exists / deleted / deactivated / disallowed
				// Soft fails → free mailbox for retry.
				switch {
				case mail.IsGraphAuthPermanent(regErr):
					opt.Pool.MarkGraphDead(mb)
					opt.log("%s REGISTER FAIL (graph dead · marked used): %v", tag, regErr)
				case register.IsEmailPermanentlyUnusable(regErr):
					opt.Pool.Mark(mb, true) // burn this alias only — free would waste another OTP cycle
					reason := "email unusable"
					switch {
					case register.IsEmailAlreadyRegistered(regErr):
						reason = "email already registered"
					case strings.Contains(strings.ToLower(regErr.Error()), "deleted") ||
						strings.Contains(strings.ToLower(regErr.Error()), "deactivated"):
						reason = "email deleted/deactivated"
					}
					opt.log("%s REGISTER FAIL (%s · marked used): %v", tag, reason, regErr)
				default:
					opt.Pool.Mark(mb, false) // release in_use → free again
					opt.log("%s REGISTER FAIL (mailbox freed, not used): %v", tag, regErr)
				}
				atomic.AddInt64(&stats.Fail, 1)
				continue
			}
			opt.Pool.Mark(mb, true)
			atomic.AddInt64(&stats.Registered, 1)
			opt.log("%s registered · AT=%v RT=%v ST=%v · src=%s · proxy=%s",
				tag, reg.AccessToken != "", reg.RefreshToken != "", reg.SessionToken != "",
				reg.SourceType, maskProxy(proxy))

			acc := storage.Account{
				"email":         reg.Email,
				"password":      reg.Password,
				"access_token":  reg.AccessToken,
				"refresh_token": reg.RefreshToken,
				"id_token":      reg.IDToken,
				"source_type":   reg.SourceType,
				"created_at":    reg.CreatedAt,
			}
			if reg.SessionToken != "" {
				acc["session_token"] = reg.SessionToken
			}
			// Hint for consumers: which OAuth client produced the final AT.
			switch {
			case reg.SourceType == "platform" || strings.HasPrefix(reg.SourceType, "platform"):
				acc["client_id"] = "app_2SKx67EdpoN0G6j64rFvigXD"
				acc["oauth_path"] = "platform"
			case strings.HasPrefix(reg.SourceType, "chatgpt_web"):
				acc["client_id"] = "app_X8zY6vW2pQ9tR3dE7nK1jL5gH"
				acc["oauth_path"] = "chatgpt_web"
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

				approveOK := !cfg.ApproveRequests || !hasMgr
				if hasMgr && cfg.ApproveRequests && joinStatus(joins) == "ok" {
					opt.log("%s approve · %s …", tag, reg.Email)
					if err := workspace.ApproveByEmail(mgr, reg.Email, proxy, cfg.ApproveMaxAttempts); err != nil {
						opt.log("%s approve fail: %v", tag, err)
						acc["approve_status"] = "failed"
						approveOK = false
					} else {
						opt.log("%s approve ok", tag)
						acc["approve_status"] = "ok"
						approveOK = true
						atomic.AddInt64(&stats.ApproveOK, 1)
						// Membership propagation after PATCH.
						if err := sleepCtx(ctx, 4*time.Second); err != nil {
							return
						}
					}
				} else if hasMgr && cfg.ApproveRequests {
					acc["approve_status"] = "skipped"
					approveOK = false
				}

				// Elevate → real k12 JWT via session cookie (not check-API label alone).
				// Skip when approve failed — free JWT is guaranteed and wastes multi-pass time.
				at := reg.AccessToken
				st := reg.SessionToken
				if st == "" {
					st = str(acc["session_token"])
				}
				preferred := cfg.ActiveWorkspaceIDs()
				if len(preferred) == 0 && activeWID != "" {
					preferred = []string{activeWID}
				}
				elevatedOK := false
				canElevate := joinStatus(joins) == "ok" && st != "" && len(preferred) > 0 && approveOK
				if joinStatus(joins) == "ok" && st != "" && len(preferred) > 0 && !approveOK && cfg.ApproveRequests {
					acc["elevate_status"] = "skipped_no_approve"
					opt.log("%s elevate skip · approve not ok (would stay free)", tag)
				} else if canElevate {
					maxPass := 2
					for pass := 1; pass <= maxPass; pass++ {
						if ctx.Err() != nil {
							return
						}
						opt.log("%s elevate → k12 · pass %d/%d · ST=yes · ws=%s",
							tag, pass, maxPass, trunc(preferred[0], 10))
						fields, e := workspace.ElevateSession(st, preferred, proxy)
						if e != nil {
							opt.log("%s elevate fail · pass %d: %v", tag, pass, e)
						} else if fields.AccessToken != "" {
							at = fields.AccessToken
							reg.AccessToken = fields.AccessToken
							acc["access_token"] = fields.AccessToken
							if fields.SessionToken != "" {
								st = fields.SessionToken
								reg.SessionToken = fields.SessionToken
								acc["session_token"] = fields.SessionToken
							}
							if fields.ChatGPTAccountID != "" {
								acc["chatgpt_account_id"] = fields.ChatGPTAccountID
							}
							if fields.PlanType != "" {
								acc["plan_type"] = fields.PlanType
							}
							if fields.Expires != "" {
								acc["expires"] = fields.Expires
							}
							if workspace.JWTIsWorkspaceScoped(at) {
								acc["workspace_scope"] = "elevated"
								acc["elevate_status"] = "ok"
								if jp := workspace.JWTPlanType(at); jp != "" {
									acc["plan_type"] = jp
								}
								elevatedOK = true
								opt.log("%s elevate ok · plan=%s jwt=%s id=%s",
									tag,
									str(acc["plan_type"]),
									workspace.JWTPlanType(at),
									trunc(str(acc["chatgpt_account_id"]), 12),
								)
								break
							}
							opt.log("%s elevate AT but jwt still free · plan=%s", tag, fields.PlanType)
						}
						// Membership lag: light re-approve + short wait.
						if pass < maxPass && hasMgr && cfg.ApproveRequests {
							_ = workspace.ApproveByEmail(mgr, reg.Email, proxy, 2)
							if err := sleepCtx(ctx, time.Duration(pass)*3*time.Second); err != nil {
								return
							}
						}
					}
					if !elevatedOK {
						acc["elevate_status"] = "failed"
						opt.log("%s elevate not k12 yet (will check API · jwt may stay free)", tag)
					}
				} else if joinStatus(joins) == "ok" && st == "" {
					acc["elevate_status"] = "no_session"
					opt.log("%s no session_token · cannot cookie-elevate · JWT stays free until ST exists", tag)
					if reg.RefreshToken != "" {
						if newAT, newRT, e := workspace.RefreshTokens(reg.RefreshToken, proxy); e == nil && newAT != "" {
							at = newAT
							reg.AccessToken = newAT
							acc["access_token"] = newAT
							if newRT != "" {
								reg.RefreshToken = newRT
								acc["refresh_token"] = newRT
							}
							opt.log("%s token refresh ok (still need ST for k12 JWT)", tag)
						} else if e != nil {
							opt.log("%s token refresh: %v", tag, e)
						}
					}
				}

				// Live AT probe: JWT k12 alone is not enough for Codex — OpenAI may
				// already have token_invalidated the session while the JWT still decodes.
				aidHint := str(acc["chatgpt_account_id"])
				if aidHint == "" {
					aidHint = activeWID
				}
				tokenLive := false
				if at != "" {
					if pe := workspace.ProbeAccessToken(at, aidHint, proxy); pe == nil {
						tokenLive = true
						acc["token_status"] = "live"
						opt.log("%s token probe ok · plan=%s", tag, workspace.JWTPlanType(at))
					} else {
						acc["token_status"] = "dead"
						opt.log("%s token probe FAIL · %v", tag, pe)
						// Recover: RT refresh → re-elevate with ST → probe again.
						if reg.RefreshToken != "" {
							if newAT, newRT, e := workspace.RefreshTokens(reg.RefreshToken, proxy); e == nil && newAT != "" {
								at = newAT
								reg.AccessToken = newAT
								acc["access_token"] = newAT
								if newRT != "" {
									reg.RefreshToken = newRT
									acc["refresh_token"] = newRT
								}
								opt.log("%s recovered AT via RT refresh", tag)
							} else if e != nil {
								opt.log("%s RT refresh: %v", tag, e)
							}
						}
						if st != "" && len(preferred) > 0 {
							if fields, e := workspace.ElevateSession(st, preferred, proxy); e == nil && fields.AccessToken != "" {
								at = fields.AccessToken
								reg.AccessToken = fields.AccessToken
								acc["access_token"] = fields.AccessToken
								if fields.SessionToken != "" {
									st = fields.SessionToken
									reg.SessionToken = fields.SessionToken
									acc["session_token"] = fields.SessionToken
								}
								if fields.ChatGPTAccountID != "" {
									acc["chatgpt_account_id"] = fields.ChatGPTAccountID
									aidHint = fields.ChatGPTAccountID
								}
								if workspace.JWTIsWorkspaceScoped(at) {
									elevatedOK = true
									acc["elevate_status"] = "ok"
									acc["workspace_scope"] = "elevated"
									acc["plan_type"] = workspace.JWTPlanType(at)
									opt.log("%s re-elevate after probe · plan=%s", tag, acc["plan_type"])
								}
							} else if e != nil {
								opt.log("%s re-elevate: %v", tag, e)
							}
						}
						if pe2 := workspace.ProbeAccessToken(at, aidHint, proxy); pe2 == nil {
							tokenLive = true
							acc["token_status"] = "live"
							opt.log("%s token probe ok after recover", tag)
						} else {
							acc["token_status"] = "invalidated"
							elevatedOK = false
							opt.log("%s token still dead after recover · %v · skip import", tag, pe2)
						}
					}
				}

				// Check API: membership metadata only. Do NOT treat as k12 success
				// unless JWT is workspace-scoped AND token is live.
				if plan, aid, err := workspace.CheckPlan(at, proxy, preferred); err == nil {
					if aid != "" {
						acc["chatgpt_account_id"] = aid
					}
					jwtScoped := workspace.JWTIsWorkspaceScoped(at)
					jwtPlan := workspace.JWTPlanType(at)
					if jwtScoped {
						if jwtPlan != "" {
							acc["plan_type"] = jwtPlan
						} else if plan != "" {
							acc["plan_type"] = plan
						}
						if acc["workspace_scope"] != "elevated" {
							acc["workspace_scope"] = "check"
						}
					} else {
						if plan != "" {
							acc["check_plan_type"] = plan
							// Keep free/empty plan_type when JWT is not scoped —
							// avoids false k12 stats/import from check-only labels.
							if str(acc["plan_type"]) == "" {
								acc["plan_type"] = "free"
							}
						}
						if workspace.IsWorkspacePlan(plan) {
							acc["workspace_scope"] = "check_pending"
						}
					}
					opt.log("%s plan=%s check=%s jwt=%s live=%v account=%s",
						tag, str(acc["plan_type"]), plan, jwtOrFree(jwtPlan, jwtScoped), tokenLive, trunc(aid, 12))
					if tokenLive && (elevatedOK || jwtScoped) {
						atomic.AddInt64(&stats.K12, 1)
					}
				} else {
					opt.log("%s plan check: %v", tag, err)
					if workspace.IsTokenInvalidated(err) {
						tokenLive = false
						acc["token_status"] = "invalidated"
						elevatedOK = false
					}
					// Only count k12 when JWT scoped AND live probe passed.
					if tokenLive && (elevatedOK || workspace.JWTIsWorkspaceScoped(at)) {
						atomic.AddInt64(&stats.K12, 1)
					}
				}
				// Stash for import gate below (same worker scope).
				acc["_token_live"] = tokenLive
			}

			// No-workspace (or join skipped): still probe AT before import/write.
			if _, hasLive := acc["_token_live"]; !hasLive && reg.AccessToken != "" {
				aidHint := str(acc["chatgpt_account_id"])
				if pe := workspace.ProbeAccessToken(reg.AccessToken, aidHint, proxy); pe == nil {
					acc["_token_live"] = true
					acc["token_status"] = "live"
					opt.log("%s token probe ok", tag)
				} else {
					opt.log("%s token probe FAIL · %v", tag, pe)
					if reg.RefreshToken != "" {
						if newAT, newRT, e := workspace.RefreshTokens(reg.RefreshToken, proxy); e == nil && newAT != "" {
							reg.AccessToken = newAT
							acc["access_token"] = newAT
							if newRT != "" {
								reg.RefreshToken = newRT
								acc["refresh_token"] = newRT
							}
							if pe2 := workspace.ProbeAccessToken(reg.AccessToken, aidHint, proxy); pe2 == nil {
								acc["_token_live"] = true
								acc["token_status"] = "live"
								opt.log("%s token probe ok after RT refresh", tag)
							} else {
								acc["_token_live"] = false
								acc["token_status"] = "invalidated"
								opt.log("%s token still dead · %v", tag, pe2)
							}
						} else {
							acc["_token_live"] = false
							acc["token_status"] = "invalidated"
						}
					} else {
						acc["_token_live"] = false
						acc["token_status"] = "invalidated"
					}
				}
			}

			// tokenLive for AT write / import gates.
			tokenLive := true
			if v, ok := acc["_token_live"].(bool); ok {
				tokenLive = v
				delete(acc, "_token_live")
			}

			// Prefer writing token when JWT is real k12 (or always if no workspace).
			// Never write dead/invalidated tokens into access_token.txt for Codex use.
			writeTok := reg.AccessToken
			if v, ok := acc["token_status"].(string); ok && (v == "dead" || v == "invalidated") {
				writeTok = ""
			}
			if !tokenLive {
				writeTok = ""
			}
			if cfg.WorkspaceEnabled && cfg.ImportRequireK12 {
				if !workspace.JWTIsWorkspaceScoped(writeTok) && !isK12(str(acc["plan_type"])) {
					writeTok = "" // still saved in JSON; skip free AT line
				}
				// Stricter: if we require k12 for import, only write JWT-scoped tokens.
				if !workspace.JWTIsWorkspaceScoped(reg.AccessToken) {
					writeTok = ""
				}
			}

			importEps := cfg.ActiveImportEndpoints()
			needAgent := cfg.CodexAgentEnabled || hasAgentIdentityEndpoint(importEps)
			var agentAuth *codexagent.AuthJSON
			// Register Codex Agent Identity when enabled or any import endpoint needs it.
			if writeTok != "" && needAgent {
				auth, err := registerCodexAgent(cfg, writeTok, proxy, acc, tag, opt.log)
				if err != nil {
					opt.log("%s codex agent: %v", tag, err)
					acc["codex_agent_status"] = "failed"
				} else {
					agentAuth = auth
				}
			}

			// Optional import to one or more account pools.
			// Mode "at": push access_token (live JWT, optional k12 gate).
			// Mode "agent_identity": push auth.json to codex2api (AGENT_IDENTITY_IMPORT.md).
			if len(importEps) > 0 {
				plan := strings.ToLower(str(acc["plan_type"]))
				jwtOK := workspace.JWTIsWorkspaceScoped(reg.AccessToken)
				var results []map[string]any
				okN, failN, skipN := 0, 0, 0
				for _, ep := range importEps {
					label := ep.Name
					if label == "" {
						label = ep.URL
					}
					mode := importapi.NormalizeMode(ep.Mode)
					reqK12 := ep.RequireK12

					if mode == importapi.ModeAgentIdentity {
						if agentAuth == nil {
							skipN++
							results = append(results, map[string]any{
								"name": label, "url": ep.URL, "mode": mode, "status": "skipped",
								"reason": "no_agent_auth",
							})
							opt.log("%s import skip · %s · agent_identity · no auth.json", tag, label)
							continue
						}
						if reqK12 && !jwtOK {
							skipN++
							results = append(results, map[string]any{
								"name": label, "url": ep.URL, "mode": mode, "status": "skipped",
								"reason": "jwt_not_k12 plan=" + plan,
							})
							opt.log("%s import skip · %s · agent_identity · JWT not k12 plan=%s", tag, label, plan)
							continue
						}
						// Admin API direct (no reg proxy). Optional ep.ProxyURL is gateway-side proxy for the account.
						name := str(acc["email"])
						if name == "" {
							name = agentAuth.AgentIdentity.Email
						}
						ir := importapi.PushAgentIdentity(ep.URL, ep.AdminKey, agentAuth, name, ep.ProxyURL, "")
						if !ir.OK && isImportNetErr(ir.Error) {
							if err := sleepCtx(ctx, 800*time.Millisecond); err != nil {
								return
							}
							ir = importapi.PushAgentIdentity(ep.URL, ep.AdminKey, agentAuth, name, ep.ProxyURL, "")
						}
						entry := map[string]any{
							"name": label, "url": ep.URL, "mode": mode, "status": ir.Outcome, "ok": ir.OK,
						}
						if ir.Error != "" {
							entry["error"] = ir.Error
						}
						if ir.Email != "" {
							entry["email"] = ir.Email
						}
						results = append(results, entry)
						if ir.OK {
							okN++
							opt.log("%s import ok · %s · agent_identity · %s", tag, label, ir.Outcome)
						} else {
							failN++
							opt.log("%s import fail · %s · agent_identity · %s", tag, label, ir.Error)
						}
						continue
					}

					// Mode AT (legacy)
					if reg.AccessToken == "" {
						skipN++
						results = append(results, map[string]any{
							"name": label, "url": ep.URL, "mode": mode, "status": "skipped",
							"reason": "no_access_token",
						})
						continue
					}
					if !tokenLive {
						skipN++
						results = append(results, map[string]any{
							"name": label, "url": ep.URL, "mode": mode, "status": "skipped",
							"reason": "token_invalidated",
						})
						opt.log("%s import skip · %s · at · token not live", tag, label)
						continue
					}
					if reqK12 && !jwtOK {
						skipN++
						results = append(results, map[string]any{
							"name": label, "url": ep.URL, "mode": mode, "status": "skipped",
							"reason": "jwt_not_k12 plan=" + plan,
						})
						opt.log("%s import skip · %s · at · JWT not workspace-scoped plan=%s", tag, label, plan)
						continue
					}
					ir := importapi.Push(ep.URL, ep.AdminKey, reg.AccessToken, "")
					if !ir.OK && isImportNetErr(ir.Error) {
						if err := sleepCtx(ctx, 800*time.Millisecond); err != nil {
							return
						}
						ir = importapi.Push(ep.URL, ep.AdminKey, reg.AccessToken, "")
					}
					entry := map[string]any{
						"name": label, "url": ep.URL, "mode": mode, "status": ir.Outcome, "ok": ir.OK,
					}
					if ir.Error != "" {
						entry["error"] = ir.Error
					}
					results = append(results, entry)
					if ir.OK {
						okN++
						opt.log("%s import ok · %s · at · %s", tag, label, ir.Outcome)
					} else {
						failN++
						opt.log("%s import fail · %s · at · %s", tag, label, ir.Error)
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
			OAuthPath:     cfg.OAuthPath,
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
		"graph auth failed", "aadsts70000",
		"compromised", "registration_disallowed", "user_already",
		"domain likely banned", "turnstile.dx", "pool exhausted",
		// Already waited full OTP window — re-running wastes another timeout.
		"otp timeout after",
		// Identity permanently unusable — proxy rotate will not help.
		"deleted or deactivated", "has been deleted", "has been deactivated",
		"you do not have an account because", "account is deactivated",
		// create_account already succeeded server-side; full re-auth soft-retry
		// turns "verification" into "login" + invalid_state / deactivated.
		"redeem_after_create",
		"token_exchange_user_error",
		"chatgpt_web missing access_token",
	}
	for _, k := range hard {
		if strings.Contains(s, k) {
			return false
		}
	}
	// validate 409 invalid_state is often sticky-session flake — allow proxy/re-auth retry.
	// Other validate_otp (wrong code etc.) already covered above or treated soft only if network.
	if strings.Contains(s, "validate_otp") && !strings.Contains(s, "invalid_state") &&
		!strings.Contains(s, "session is no longer valid") {
		return false
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
		"platform_authorize", "chatgpt_authorize", "user_register",
		"sentinel req", "sentinel_req", "auth/session", "callback",
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
	return p == "k12" || p == "team" || p == "enterprise" || p == "edu" || p == "business"
}

func hasAgentIdentityEndpoint(eps []config.ImportEndpoint) bool {
	for _, ep := range eps {
		if importapi.NormalizeMode(ep.Mode) == importapi.ModeAgentIdentity {
			return true
		}
	}
	return false
}

// registerCodexAgent creates Codex CLI agent_identity auth.json under data/codex_auth/.
func registerCodexAgent(cfg config.Config, accessToken, proxy string, acc map[string]any, tag string, logf func(string, ...any)) (*codexagent.AuthJSON, error) {
	auth, err := codexagent.Create(codexagent.Options{
		AccessToken: accessToken,
		Proxy:       proxy,
		VerifyTask:  cfg.CodexAgentVerifyTask,
	})
	if err != nil {
		return nil, err
	}
	outDir := strings.TrimSpace(cfg.CodexAgentOutputDir)
	if outDir == "" {
		outDir = "codex_auth"
	}
	dir := cfg.ResolvePath(outDir)
	email := auth.AgentIdentity.Email
	if email == "" {
		email = str(acc["email"])
	}
	base := codexagent.SafeFilename(email)
	if base == "" || base == "unknown" {
		base = codexagent.SafeFilename(auth.AgentIdentity.AccountID)
	}
	path := filepath.Join(dir, base+".json")
	if err := codexagent.WriteAuthJSON(path, auth); err != nil {
		return nil, err
	}
	_ = codexagent.AppendAuthJSONL(filepath.Join(dir, "agents.jsonl"), auth)
	acc["codex_agent_status"] = "ok"
	acc["agent_runtime_id"] = auth.AgentIdentity.AgentRuntimeID
	acc["codex_auth_file"] = filepath.ToSlash(filepath.Join(outDir, base+".json"))
	if logf != nil {
		logf("%s codex agent ok · runtime=%s · %s", tag, trunc(auth.AgentIdentity.AgentRuntimeID, 24), path)
	}
	return auth, nil
}

func jwtOrFree(jwtPlan string, scoped bool) string {
	if scoped && jwtPlan != "" {
		return jwtPlan
	}
	if scoped {
		return "ok"
	}
	if jwtPlan != "" {
		return jwtPlan
	}
	return "free"
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
