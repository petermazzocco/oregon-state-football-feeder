# oregon-state-football-feeder

Mirrors posts from [@BeaverFootball on X](https://x.com/BeaverFootball) to the Bluesky bot account **@beaverfootball.gobeavs.app**.

It's written in Go with [indigo](https://github.com/bluesky-social/indigo), the official Bluesky Go SDK, and reads from X through the official X API v2.

## What it does

- Checks X every `POLL_INTERVAL` (5 minutes by default) and posts new tweets oldest-first.
- Turns `t.co` links back into real URLs as clickable link facets. `#hashtags` become Bluesky tags, and `@mentions` link to the X profile.
- Re-uploads photos (up to 4, with alt text, shrunk below Bluesky's 1 MB limit), videos and GIFs (through Bluesky's video service), and link cards.
- Posts the account's own reply threads as Bluesky threads. Retweets and replies to other accounts are skipped.
- Shortens posts longer than 300 graphemes and adds a "Full post on X" link.
- Adds Bluesky's **`bot` self-label** to the profile on startup. This is the same as turning on Settings → Account → *Automation label* in the app, and it puts a bot badge on the profile and posts.
- Saves state to `/data/state.json` after every post, so restarts and redeploys never post the same thing twice.

## One-time setup

### 1. Create the Bluesky account and set the custom handle
Bots can't sign up through the API because sign-up needs email verification and a captcha, so do this part by hand:

1. Sign up at bsky.app. Any temporary handle is fine.
2. Settings → Account → Handle → *I have my own domain* → enter `beaverfootball.gobeavs.app`.
3. In the DNS for `gobeavs.app`, add the TXT record it shows you:
   `_atproto.beaverfootball` → `did=did:plc:…`
   (Or serve the DID at `https://beaverfootball.gobeavs.app/.well-known/atproto-did`.)
4. Click Verify.
5. Settings → Privacy and security → **App passwords** → create one. This becomes `BSKY_APP_PASSWORD`.

### 2. Get an X API bearer token
Go to developer.x.com → create a Project/App → Keys and tokens → **Bearer Token**.

The X API is now pay-per-use, with no free read tier for new developers. Each post read costs about $0.005, and you're only charged for posts that actually come back, so polling with nothing new costs essentially nothing. Even a busy game-day account should run a few dollars a month at most. Set a spend cap in the developer console.

## Deploy on Dokploy

1. **Create Application** → Source: your Git repo → Build Type: **Dockerfile** (path `./Dockerfile`).
2. **Environment** tab: paste in the values from `.env.example`. Only these are required:
   ```
   BSKY_HANDLE=beaverfootball.gobeavs.app
   BSKY_APP_PASSWORD=xxxx-xxxx-xxxx-xxxx
   X_BEARER_TOKEN=...
   ```
3. **Advanced → Volumes**: add a **Volume Mount** (a named volume) with mount path `/data`. Without it, the app forgets which tweets it already mirrored on every redeploy.
   - If you use a *bind* mount instead, make the host folder writable by UID 65532 (`chown 65532:65532 <dir>`).
4. Leave Domains empty, because this is a worker. Port 8080 only serves `/healthz`, which the Docker `HEALTHCHECK` uses.
5. Deploy.

**Suggested first deploy:** set `DRY_RUN=true` and read the logs. Then set it back to `false`. Only tweets posted after the switch get mirrored, because `BACKFILL_COUNT=0` starts from "now". To seed the feed with the latest few tweets on the very first run, set `BACKFILL_COUNT=5` (before any state file exists).

## Environment variables

| Var | Default | Notes |
|---|---|---|
| `BSKY_HANDLE` | — | **required** |
| `BSKY_APP_PASSWORD` | — | **required**, use an app password |
| `X_BEARER_TOKEN` | — | **required** |
| `X_USERNAME` | `BeaverFootball` | account to mirror |
| `X_USER_ID` | | optional, skips the username lookup |
| `BSKY_PDS_HOST` | `https://bsky.social` | |
| `BSKY_SET_BOT_LABEL` | `true` | apply the `bot` self-label |
| `POLL_INTERVAL` | `5m` | minimum `1m` |
| `MIRROR_THREADS` | `true` | mirror self-reply threads |
| `BACKFILL_COUNT` | `0` | first run only |
| `MAX_ATTEMPTS` | `3` | then the tweet is skipped |
| `DRY_RUN` | `false` | log instead of posting |
| `POST_LANG` | `en` | |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `STATE_PATH` | `/data/state.json` | |
| `HEALTH_ADDR` | `:8080` | empty disables it |

## Local development

```sh
cp .env.example .env   # fill in
set -a; source .env; set +a
STATE_PATH=./data/state.json DRY_RUN=true go run . -once
go test ./...
```

## Why the official API instead of scraping?
Most open-source X→Bluesky mirrors scrape X with logged-in session cookies (for example tweets2bsky and tw-bsky). That breaks whenever X changes its site, and it puts the scraping account at risk. The pay-per-use API costs pennies for one account, so it's the reliable choice for an always-on bot.
