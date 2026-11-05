// Command oregon-state-football-feeder mirrors an X (Twitter) account's posts to a
// Bluesky account that is self-labeled as a bot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (for Docker HEALTHCHECK)")
	once := flag.Bool("once", false, "run a single poll and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(probeHealth())
	}

	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	setupLogging(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, *once); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
	slog.Info("shut down")
}

func run(ctx context.Context, cfg *Config, once bool) error {
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return fmt.Errorf("load state %s: %w", cfg.StatePath, err)
	}

	b, err := loginWithRetry(ctx, cfg)
	if err != nil {
		return err
	}
	slog.Info("logged in to bluesky", "handle", cfg.BskyHandle, "did", b.DID(), "pds", b.pdsHost)

	if cfg.SetBotLabel && !cfg.DryRun {
		if err := b.EnsureBotLabel(ctx); err != nil {
			slog.Error("failed to apply bot label (continuing)", "err", err)
		}
	}

	m := &Mirror{cfg: cfg, x: NewXClient(cfg.XBearerToken), bsky: b, state: state, lastSuccess: time.Now()}

	if cfg.HealthAddr != "" && !once {
		go serveHealth(cfg.HealthAddr, m)
	}

	slog.Info("starting mirror", "x_user", cfg.XUsername, "interval", cfg.PollInterval,
		"threads", cfg.MirrorThreads, "dry_run", cfg.DryRun)

	for {
		wait := cfg.PollInterval
		err := m.Poll(ctx)
		var rl *RateLimitError
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			return err
		case errors.As(err, &rl):
			wait = max(time.Until(rl.Reset)+5*time.Second, cfg.PollInterval)
			slog.Warn("rate limited by X; backing off", "until", rl.Reset)
		case isAuthError(err):
			slog.Warn("bluesky session invalid; logging in again", "err", err)
			if nb, lerr := loginWithRetry(ctx, cfg); lerr == nil {
				m.bsky = nb
			}
		default:
			slog.Error("poll failed", "err", err)
		}
		if once {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func loginWithRetry(ctx context.Context, cfg *Config) (*Bsky, error) {
	delay := 10 * time.Second
	for attempt := 1; ; attempt++ {
		b, err := NewBsky(ctx, cfg.BskyPDSHost, cfg.BskyHandle, cfg.BskyAppPassword)
		if err == nil {
			return b, nil
		}
		// Bad credentials won't fix themselves; fail fast so the deploy shows an error.
		var apiErr *atclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
			return nil, err
		}
		if attempt >= 5 {
			return nil, err
		}
		slog.Warn("bluesky login failed; retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func isAuthError(err error) bool {
	var apiErr *atclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusUnauthorized ||
		apiErr.Name == "ExpiredToken" || apiErr.Name == "InvalidToken"
}

func setupLogging(level string) {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}

// serveHealth reports unhealthy if X hasn't been polled successfully in a while.
func serveHealth(addr string, m *Mirror) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		stale := time.Since(m.lastSuccess) > 4*m.cfg.PollInterval+time.Hour
		if stale {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "stale: last successful poll %s\n", m.lastSuccess.Format(time.RFC3339))
			return
		}
		fmt.Fprintf(w, "ok: last successful poll %s, since_id %s\n", m.lastSuccess.Format(time.RFC3339), m.state.SinceID)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("health server stopped", "err", err)
	}
}

func probeHealth() int {
	addr := env("HEALTH_ADDR", ":8080")
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
