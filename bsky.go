package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	lexutil "github.com/bluesky-social/indigo/lex/util"
)

const videoServiceHost = "https://video.bsky.app"

type Bsky struct {
	client  *atclient.APIClient
	did     syntax.DID
	pdsHost string // the account's actual PDS (needed as audience for video service auth)
	http    *http.Client
}

func NewBsky(ctx context.Context, host, handle, appPassword string) (*Bsky, error) {
	c, err := atclient.LoginWithPasswordHost(ctx, host, handle, appPassword, "", nil)
	if err != nil {
		return nil, fmt.Errorf("bluesky login: %w", err)
	}
	c.Headers.Set("User-Agent", "oregon-state-football-feeder/1.0")
	b := &Bsky{client: c, did: *c.AccountDID, pdsHost: host, http: &http.Client{Timeout: 5 * time.Minute}}

	// Resolve the real PDS endpoint (bsky.social is an entryway in front of many PDSes).
	if ident, err := identity.DefaultDirectory().LookupDID(ctx, b.did); err == nil {
		if ep := ident.PDSEndpoint(); ep != "" {
			b.pdsHost = ep
		}
	} else {
		slog.Warn("could not resolve PDS endpoint; video uploads may fail", "err", err)
	}
	return b, nil
}

func (b *Bsky) DID() string { return b.did.String() }

// EnsureBotLabel adds the "bot" self-label to the profile record. This is the
// same thing the Bluesky app's Settings -> Account -> "Automation label" toggle does,
// and shows a bot badge on the profile and posts.
//
// The profile is read and written as raw JSON so fields this code doesn't know about
// (avatar, banner, pinned post, ...) are preserved.
func (b *Bsky) EnsureBotLabel(ctx context.Context) error {
	var got struct {
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	err := b.client.Get(ctx, "com.atproto.repo.getRecord", map[string]any{
		"repo": b.DID(), "collection": "app.bsky.actor.profile", "rkey": "self",
	}, &got)
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) && apiErr.Name == "RecordNotFound" {
		got.Value = map[string]any{"$type": "app.bsky.actor.profile"}
	} else if err != nil {
		return fmt.Errorf("get profile: %w", err)
	}

	labels, _ := got.Value["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{"$type": "com.atproto.label.defs#selfLabels", "values": []any{}}
	}
	values, _ := labels["values"].([]any)
	for _, v := range values {
		if m, ok := v.(map[string]any); ok && m["val"] == "bot" {
			slog.Info("profile already has bot label")
			return nil
		}
	}
	labels["values"] = append(values, map[string]any{"val": "bot"})
	got.Value["labels"] = labels

	in := map[string]any{
		"repo": b.DID(), "collection": "app.bsky.actor.profile", "rkey": "self", "record": got.Value,
	}
	if got.CID != "" {
		in["swapRecord"] = got.CID
	}
	if err := b.client.Post(ctx, "com.atproto.repo.putRecord", in, nil); err != nil {
		return fmt.Errorf("put profile: %w", err)
	}
	slog.Info("applied bot label to profile")
	return nil
}

// UploadBlob uploads image bytes to the PDS.
func (b *Bsky) UploadBlob(ctx context.Context, data []byte, mime string) (*lexutil.LexBlob, error) {
	req := atclient.NewAPIRequest(http.MethodPost, "com.atproto.repo.uploadBlob", bytes.NewReader(data))
	req.Headers.Set("Content-Type", mime)
	req.Headers.Set("Accept", "application/json")
	resp, err := b.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("uploadBlob: %s: %s", resp.Status, truncate(string(body), 300))
	}
	var out comatproto.RepoUploadBlob_Output
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Blob, nil
}

// UploadVideo uses Bluesky's video service, which transcodes the video and
// returns a blob ref usable in app.bsky.embed.video.
// Flow: getServiceAuth (aud = PDS DID, lxm = uploadBlob) -> video.bsky.app uploadVideo -> poll getJobStatus.
func (b *Bsky) UploadVideo(ctx context.Context, data []byte, name string) (*lexutil.LexBlob, error) {
	aud := "did:web:" + strings.TrimPrefix(strings.TrimPrefix(b.pdsHost, "https://"), "http://")
	var sa struct {
		Token string `json:"token"`
	}
	err := b.client.Get(ctx, "com.atproto.server.getServiceAuth", map[string]any{
		"aud": aud,
		"lxm": "com.atproto.repo.uploadBlob",
		"exp": time.Now().Add(30 * time.Minute).Unix(),
	}, &sa)
	if err != nil {
		return nil, fmt.Errorf("getServiceAuth: %w", err)
	}

	q := url.Values{"did": {b.DID()}, "name": {name}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		videoServiceHost+"/xrpc/app.bsky.video.uploadVideo?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sa.Token)
	req.Header.Set("Content-Type", "video/mp4")
	req.ContentLength = int64(len(data))

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var job struct {
		JobID string           `json:"jobId"`
		State string           `json:"state"`
		Error string           `json:"error"`
		Blob  *lexutil.LexBlob `json:"blob"`
	}
	_ = json.Unmarshal(body, &job)
	// 409 "already_exists" still returns the jobId, which we can poll.
	if resp.StatusCode != http.StatusOK && !(resp.StatusCode == http.StatusConflict && job.JobID != "") {
		return nil, fmt.Errorf("uploadVideo: %s: %s", resp.Status, truncate(string(body), 300))
	}
	if job.Blob != nil {
		return job.Blob, nil
	}

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var st struct {
			JobStatus struct {
				State    string           `json:"state"`
				Progress int              `json:"progress"`
				Error    string           `json:"error"`
				Message  string           `json:"message"`
				Blob     *lexutil.LexBlob `json:"blob"`
			} `json:"jobStatus"`
		}
		vc := atclient.NewAPIClient(videoServiceHost)
		if err := vc.Get(ctx, "app.bsky.video.getJobStatus", map[string]any{"jobId": job.JobID}, &st); err != nil {
			return nil, fmt.Errorf("getJobStatus: %w", err)
		}
		switch st.JobStatus.State {
		case "JOB_STATE_COMPLETED":
			if st.JobStatus.Blob == nil {
				return nil, fmt.Errorf("video job completed without blob")
			}
			return st.JobStatus.Blob, nil
		case "JOB_STATE_FAILED":
			return nil, fmt.Errorf("video processing failed: %s %s", st.JobStatus.Error, st.JobStatus.Message)
		}
	}
	return nil, fmt.Errorf("video processing timed out")
}

// CreatePost creates an app.bsky.feed.post and returns its strong ref.
func (b *Bsky) CreatePost(ctx context.Context, post *bsky.FeedPost) (*comatproto.RepoStrongRef, error) {
	post.LexiconTypeID = "app.bsky.feed.post"
	out, err := comatproto.RepoCreateRecord(ctx, b.client, &comatproto.RepoCreateRecord_Input{
		Repo:       b.DID(),
		Collection: "app.bsky.feed.post",
		Record:     &lexutil.LexiconTypeDecoder{Val: post},
	})
	if err != nil {
		return nil, err
	}
	return &comatproto.RepoStrongRef{Uri: out.Uri, Cid: out.Cid}, nil
}
