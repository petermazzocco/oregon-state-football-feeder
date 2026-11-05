package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	bsky "github.com/bluesky-social/indigo/api/bsky"
)

type Mirror struct {
	cfg   *Config
	x     *XClient
	bsky  *Bsky
	state *State

	lastSuccess time.Time
}

// Poll fetches new tweets and mirrors them oldest-first. State is saved after
// every tweet so a crash/redeploy never double-posts.
func (m *Mirror) Poll(ctx context.Context) error {
	if m.state.XUserID == "" {
		id := m.cfg.XUserID
		if id == "" {
			var err error
			if id, err = m.x.LookupUserID(ctx, m.cfg.XUsername); err != nil {
				return err
			}
		}
		m.state.XUserID = id
		if err := m.state.Save(); err != nil {
			return err
		}
		slog.Info("resolved X user", "username", m.cfg.XUsername, "id", id)
	}

	firstRun := m.state.SinceID == ""
	limit := 20
	if firstRun {
		limit = max(5, m.cfg.BackfillCount)
	}
	tl, err := m.x.UserTweets(ctx, m.state.XUserID, m.state.SinceID, m.cfg.MirrorThreads, limit)
	if err != nil {
		return err
	}
	m.lastSuccess = time.Now()

	tweets := tl.Tweets
	sort.Slice(tweets, func(i, j int) bool { return idLess(tweets[i].ID, tweets[j].ID) })

	if firstRun {
		if len(tweets) == 0 {
			slog.Info("first run: no tweets found yet")
			return nil
		}
		newest := tweets[len(tweets)-1].ID
		if m.cfg.BackfillCount <= 0 {
			m.state.SinceID = newest
			slog.Info("first run: starting from latest tweet, no backfill", "since_id", newest)
			return m.state.Save()
		}
		if len(tweets) > m.cfg.BackfillCount {
			// Mark older ones as seen by starting since_id just before the backfill window.
			m.state.SinceID = tweets[len(tweets)-m.cfg.BackfillCount-1].ID
			tweets = tweets[len(tweets)-m.cfg.BackfillCount:]
		}
		slog.Info("first run: backfilling", "count", len(tweets))
	}

	if len(tweets) > 0 {
		slog.Info("fetched new tweets", "count", len(tweets))
	}
	for _, t := range tweets {
		if err := ctx.Err(); err != nil {
			return err
		}
		log := slog.With("tweet_id", t.ID)

		if reason := m.skipReason(&t); reason != "" {
			log.Info("skipping tweet", "reason", reason)
			m.advance(t.ID)
			if err := m.state.Save(); err != nil {
				return err
			}
			continue
		}

		ref, err := m.mirrorTweet(ctx, &t, tl.Media)
		if err != nil {
			m.state.Failures[t.ID]++
			n := m.state.Failures[t.ID]
			if n < m.cfg.MaxAttempts {
				log.Error("mirror failed; will retry next poll", "attempt", n, "err", err)
				_ = m.state.Save()
				return nil // stop here so ordering is preserved
			}
			log.Error("mirror failed too many times; giving up on this tweet", "attempt", n, "err", err)
			delete(m.state.Failures, t.ID)
			m.advance(t.ID)
			if err := m.state.Save(); err != nil {
				return err
			}
			continue
		}

		delete(m.state.Failures, t.ID)
		if ref != nil {
			m.state.Mirrored[t.ID] = *ref
			log.Info("mirrored tweet", "bsky_uri", ref.URI)
		}
		m.advance(t.ID)
		if err := m.state.Save(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mirror) advance(id string) {
	if idLess(m.state.SinceID, id) {
		m.state.SinceID = id
	}
}

func (m *Mirror) skipReason(t *Tweet) string {
	if t.IsRetweet() {
		return "retweet"
	}
	if parent := t.RepliedTo(); parent != "" {
		if t.InReplyToUserID != m.state.XUserID {
			return "reply to another account"
		}
		if !m.cfg.MirrorThreads {
			return "self-reply (MIRROR_THREADS=false)"
		}
	}
	return ""
}

func (m *Mirror) tweetURL(id string) string {
	user := m.cfg.XUsername
	if user == "" {
		user = "i/web"
	}
	return fmt.Sprintf("https://x.com/%s/status/%s", user, id)
}

func (m *Mirror) mirrorTweet(ctx context.Context, t *Tweet, media map[string]XMedia) (*PostRef, error) {
	text, ents := t.FullText()
	rt := BuildRichText(text, ents, m.tweetURL(t.ID))

	post := &bsky.FeedPost{
		Text:      rt.Text,
		Facets:    rt.Facets,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if m.cfg.PostLang != "" {
		post.Langs = []string{m.cfg.PostLang}
	}

	// Thread continuity: reply to our mirrored copy of the parent tweet.
	var root *comatproto.RepoStrongRef
	if parentID := t.RepliedTo(); parentID != "" {
		if p, ok := m.state.Mirrored[parentID]; ok {
			parent := &comatproto.RepoStrongRef{Uri: p.URI, Cid: p.CID}
			root = &comatproto.RepoStrongRef{Uri: p.RootURI, Cid: p.RootCID}
			if root.Uri == "" {
				root = parent
			}
			post.Reply = &bsky.FeedPost_ReplyRef{Parent: parent, Root: root}
		}
	}

	if m.cfg.DryRun {
		slog.Info("DRY RUN: would post", "text", post.Text, "facets", len(post.Facets),
			"media_keys", mediaKeys(t), "reply", post.Reply != nil)
		return nil, nil
	}

	embed, err := m.buildEmbed(ctx, t, ents, media)
	if err != nil {
		return nil, err
	}
	post.Embed = embed

	if strings.TrimSpace(post.Text) == "" && post.Embed == nil {
		return nil, errors.New("nothing to post (empty text and no media)")
	}

	ref, err := m.bsky.CreatePost(ctx, post)
	if err != nil {
		return nil, fmt.Errorf("create post: %w", err)
	}
	out := &PostRef{URI: ref.Uri, CID: ref.Cid, RootURI: ref.Uri, RootCID: ref.Cid}
	if root != nil {
		out.RootURI, out.RootCID = root.Uri, root.Cid
	}
	return out, nil
}

func mediaKeys(t *Tweet) []string {
	if t.Attachments == nil {
		return nil
	}
	return t.Attachments.MediaKeys
}

// buildEmbed picks, in order: video/GIF, up to 4 photos, or a link card.
func (m *Mirror) buildEmbed(ctx context.Context, t *Tweet, ents *TweetEntities, media map[string]XMedia) (*bsky.FeedPost_Embed, error) {
	var photos []XMedia
	for _, k := range mediaKeys(t) {
		mm, ok := media[k]
		if !ok {
			continue
		}
		switch mm.Type {
		case "video", "animated_gif":
			e, err := m.videoEmbed(ctx, t.ID, mm)
			if err == nil {
				return e, nil
			}
			slog.Warn("video upload failed; falling back to preview image", "tweet_id", t.ID, "err", err)
			if mm.PreviewImageURL != "" {
				photos = append(photos, XMedia{Type: "photo", URL: mm.PreviewImageURL, AltText: mm.AltText})
			}
		case "photo":
			photos = append(photos, mm)
		}
	}

	if len(photos) > 0 {
		imgs := &bsky.EmbedImages{}
		for _, p := range photos[:min(4, len(photos))] {
			img, err := m.uploadPhoto(ctx, p)
			if err != nil {
				return nil, err
			}
			imgs.Images = append(imgs.Images, img)
		}
		return &bsky.FeedPost_Embed{EmbedImages: imgs}, nil
	}

	return m.linkCard(ctx, ents), nil
}

func (m *Mirror) uploadPhoto(ctx context.Context, p XMedia) (*bsky.EmbedImages_Image, error) {
	data, mime, err := download(ctx, photoURL(p.URL), 20<<20)
	if err != nil {
		return nil, err
	}
	data, mime, w, h, err := fitImage(data, mime)
	if err != nil {
		return nil, err
	}
	blob, err := m.bsky.UploadBlob(ctx, data, mime)
	if err != nil {
		return nil, err
	}
	img := &bsky.EmbedImages_Image{Alt: p.AltText, Image: blob}
	if w > 0 && h > 0 {
		img.AspectRatio = &bsky.EmbedDefs_AspectRatio{Width: w, Height: h}
	}
	return img, nil
}

func (m *Mirror) videoEmbed(ctx context.Context, tweetID string, v XMedia) (*bsky.FeedPost_Embed, error) {
	if v.DurationMS > maxVideoMS {
		return nil, fmt.Errorf("video too long (%ds)", v.DurationMS/1000)
	}
	var lastErr error = errors.New("no mp4 variants")
	for _, u := range videoCandidates(v) {
		data, _, err := download(ctx, u, maxVideoBytes)
		if err != nil {
			lastErr = err
			continue
		}
		blob, err := m.bsky.UploadVideo(ctx, data, tweetID+".mp4")
		if err != nil {
			return nil, err
		}
		ev := &bsky.EmbedVideo{Video: blob}
		if v.AltText != "" {
			ev.Alt = &v.AltText
		}
		if v.Width > 0 && v.Height > 0 {
			ev.AspectRatio = &bsky.EmbedDefs_AspectRatio{Width: v.Width, Height: v.Height}
		}
		if v.Type == "animated_gif" {
			gif := "gif"
			ev.Presentation = &gif
		}
		return &bsky.FeedPost_Embed{EmbedVideo: ev}, nil
	}
	return nil, lastErr
}

// linkCard builds an external embed from the first non-X link using the card
// metadata X already returns on the URL entity (no page scraping needed).
func (m *Mirror) linkCard(ctx context.Context, ents *TweetEntities) *bsky.FeedPost_Embed {
	if ents == nil {
		return nil
	}
	for _, u := range ents.URLs {
		full := firstNonEmpty(u.UnwoundURL, u.ExpandedURL)
		if full == "" || u.MediaKey != "" || isXURL(full) {
			continue
		}
		if u.Title == "" && u.Description == "" {
			continue
		}
		ext := &bsky.EmbedExternal_External{Uri: full, Title: u.Title, Description: u.Description}
		if len(u.Images) > 0 {
			if data, mime, err := download(ctx, u.Images[0].URL, 20<<20); err == nil {
				if data, mime, _, _, err = fitImage(data, mime); err == nil {
					if blob, err := m.bsky.UploadBlob(ctx, data, mime); err == nil {
						ext.Thumb = blob
					}
				}
			}
		}
		return &bsky.FeedPost_Embed{EmbedExternal: &bsky.EmbedExternal{External: ext}}
	}
	return nil
}

func isXURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.TrimPrefix(u.Hostname(), "www.")
	return h == "x.com" || h == "twitter.com" || h == "mobile.twitter.com"
}
