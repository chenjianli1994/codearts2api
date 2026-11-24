package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"strings"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/cli"
	"codearts2api/internal/pool"
	"codearts2api/internal/scheduler"
	"codearts2api/internal/server"
	"codearts2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("WARNING: no accounts found. Run `codearts2api login` first.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	p, err := pool.New(auths, cfg.ToPoolConfig(), cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	c := upstream.New(120 * time.Second)
	c.SetBenefitAutoClaim(cfg.BenefitAutoClaim)

	// CLI 后端：确保 codearts serve 在线（不在线就用 AK/SK 拉起）。起不来就只打
	// 一条日志并继续，直连路径照常工作（CLI 只影响 cli_backend.models 里的模型）。
	var cliClient *cli.Client
	if cfg.CLIBackend.Enabled {
		serveCfg := cli.ServeConfig{
			Exe:         cfg.CLIBackend.CodeartsExe,
			Endpoint:    cfg.CLIBackend.Endpoint,
			Credentials: cfg.CLIBackend.CredentialsFile,
			LogPath:     cfg.StateFile + ".cli-serve.log",
		}
		if err := cli.EnsureServe(context.Background(), serveCfg); err != nil {
			log.Printf("cli backend unavailable: %v (falling back to direct models only)", err)
		} else {
			timeout := time.Duration(cfg.CLIBackend.TimeoutSeconds) * time.Second
			cliClient = cli.New(cfg.CLIBackend.Endpoint, timeout)
			log.Printf("cli backend ready: %s provider=%s models=%d",
				cfg.CLIBackend.Endpoint, cfg.CLIBackend.Provider, len(cfg.CLIBackend.Models))
			if len(cfg.CLIBackend.MultimodalModels) > 0 {
				mctx, mcancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := cliClient.SetMultimodalModels(mctx, cfg.CLIBackend.MultimodalModels)
				mcancel()
				if err != nil {
					log.Printf("cli multimodal config failed: %v (images may not reach the model)", err)
				} else {
					log.Printf("cli multimodal models: %v", cfg.CLIBackend.MultimodalModels)
				}
			}
		}
	}

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		CreditsAutoClaim:  cfg.CreditsAutoClaim,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     c,
		APIKey:       cfg.APIKey,
		MaxRotate:    3,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
		QueueRetryDelay: func() time.Duration {
			if cfg.QueueRetrySeconds > 0 {
				return time.Duration(cfg.QueueRetrySeconds) * time.Second
			}
			return 10 * time.Second
		}(),
		QueueMaxAttempts: cfg.QueueMaxAttempts,
		ConvStateFile:    cfg.StateFile + ".chats.json",
		Session: server.SessionConfig{
			Enabled: cfg.Session.Enabled,
			TTL:     sessionDuration(cfg.Session.TTL, cfg.Session.TTLMinutes, 30*time.Minute),
			GC:      sessionDuration(cfg.Session.GC, cfg.Session.GCMinutes, 5*time.Minute),
		},
		WatchInfo: map[string]any{
			"enabled":              cfg.Watch.Enabled,
			"credits_auto_claim":   cfg.CreditsAutoClaim,
			"poll_minutes":         cfg.Watch.PollMinutes,
			"refresh_skew_minutes": cfg.Watch.RefreshSkewM,
			"keepalive_interval":   cfg.Watch.KeepaliveInterval,
			"max_concurrent":       cfg.MaxConcurrent,
			"keepalive_window":     "10m",
		},
		AuthDir:           cfg.AuthDir,
		Listen:            cfg.Listen,
		OAuthClient:       upstream.New(15 * time.Second),
		LoginConfig:       loginConfig(cfg),
		OAuthCallbackHost: cfg.OAuthCallbackHost,
		CLI:               cliClient,
		CLIProvider:       cfg.CLIBackend.Provider,
		CLIAgent:          cfg.CLIBackend.Agent,
		CLIModels:         cfg.CLIBackend.Models,
		CLIEnabled:        cfg.CLIBackend.Enabled,
		CLIEndpoint:       cfg.CLIBackend.Endpoint,
		CLIMultimodal:     cfg.CLIBackend.MultimodalModels,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	go h.RunConversationGC(ctx)
	h.StartAvailabilityProber(ctx)
	upstream.SetUserAgent(cfg.Outbound.UserAgent)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("codearts2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// loginConfig 决定 WebUI 登录使用的 OAuth 配置。
//
// client_id 优先取已有账号记录的取值（refresh_token 与 client_id 绑定，混用会让
// 老账号刷新失败），其次取配置项，最后才是内置默认值。其余字段必须来自
// DefaultLoginConfig（否则 authorize 链接会缺 host / plugin 信息）。
func loginConfig(cfg *Config) upstream.LoginConfig {
	lc := upstream.DefaultLoginConfig()
	if cfg.LoginClientID != "" {
		lc.ClientID = cfg.LoginClientID
	}
	if auths, err := auth.LoadDir(cfg.AuthDir); err == nil {
		for _, a := range auths {
			if id := a.ClientIDOr(""); id != "" {
				lc.ClientID = id
				break
			}
		}
	}
	return lc
}

// sessionDuration 解析会话段里的时长文本（如 "30m" / "5m" / "600s"）。
// 文本为空时回落到老的分钟字段，再不行用 fallback。
func sessionDuration(text string, legacyMinutes int, fallback time.Duration) time.Duration {
	if s := strings.TrimSpace(text); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	if legacyMinutes > 0 {
		return time.Duration(legacyMinutes) * time.Minute
	}
	return fallback
}
