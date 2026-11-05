package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is loaded entirely from environment variables so it can be managed
// from Dokploy's "Environment" tab.
type Config struct {
	// Bluesky
	BskyHandle      string // e.g. beaverfootball.gobeavs.app
	BskyAppPassword string // Settings -> Privacy and security -> App passwords
	BskyPDSHost     string // where to log in; https://bsky.social works for any bsky-hosted account
	SetBotLabel     bool   // apply the "bot" self-label (Automation label) to the profile on startup

	// X / Twitter
	XBearerToken string // X API v2 app-only bearer token
	XUsername    string // source account, without the @
	XUserID      string // optional; skips the username lookup call if set

	// Behavior
	PollInterval  time.Duration
	MirrorThreads bool // mirror the account's self-reply threads as Bluesky threads
	BackfillCount int  // on first run, how many recent posts to mirror (0 = only new posts from now on)
	MaxAttempts   int  // give up on a single post after this many failures
	DryRun        bool // log what would be posted without posting
	PostLang      string

	// Runtime
	StatePath  string
	HealthAddr string // e.g. ":8080"; empty disables the health endpoint
	LogLevel   string
}

func LoadConfig() (*Config, error) {
	c := &Config{
		BskyHandle:      strings.TrimPrefix(env("BSKY_HANDLE", ""), "@"),
		BskyAppPassword: env("BSKY_APP_PASSWORD", ""),
		BskyPDSHost:     strings.TrimRight(env("BSKY_PDS_HOST", "https://bsky.social"), "/"),
		SetBotLabel:     envBool("BSKY_SET_BOT_LABEL", true),

		XBearerToken: env("X_BEARER_TOKEN", ""),
		XUsername:    strings.TrimPrefix(env("X_USERNAME", "BeaverFootball"), "@"),
		XUserID:      env("X_USER_ID", ""),

		MirrorThreads: envBool("MIRROR_THREADS", true),
		DryRun:        envBool("DRY_RUN", false),
		PostLang:      env("POST_LANG", "en"),

		StatePath:  env("STATE_PATH", "/data/state.json"),
		HealthAddr: env("HEALTH_ADDR", ":8080"),
		LogLevel:   env("LOG_LEVEL", "info"),
	}

	var err error
	if c.PollInterval, err = time.ParseDuration(env("POLL_INTERVAL", "5m")); err != nil {
		return nil, fmt.Errorf("POLL_INTERVAL: %w", err)
	}
	if c.PollInterval < time.Minute {
		return nil, fmt.Errorf("POLL_INTERVAL must be at least 1m (X API rate limits / cost)")
	}
	if c.BackfillCount, err = envInt("BACKFILL_COUNT", 0); err != nil {
		return nil, err
	}
	if c.MaxAttempts, err = envInt("MAX_ATTEMPTS", 3); err != nil {
		return nil, err
	}

	var missing []string
	if c.BskyHandle == "" {
		missing = append(missing, "BSKY_HANDLE")
	}
	if c.BskyAppPassword == "" {
		missing = append(missing, "BSKY_APP_PASSWORD")
	}
	if c.XBearerToken == "" {
		missing = append(missing, "X_BEARER_TOKEN")
	}
	if c.XUsername == "" && c.XUserID == "" {
		missing = append(missing, "X_USERNAME or X_USER_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := env(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
