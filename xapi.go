package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Minimal X API v2 client: just what's needed to read one account's timeline.
// Docs: https://docs.x.com/x-api/users/get-posts
type XClient struct {
	bearer string
	http   *http.Client
	base   string
}

func NewXClient(bearer string) *XClient {
	return &XClient{
		bearer: bearer,
		http:   &http.Client{Timeout: 30 * time.Second},
		base:   "https://api.x.com/2",
	}
}

type Tweet struct {
	ID              string         `json:"id"`
	Text            string         `json:"text"`
	CreatedAt       time.Time      `json:"created_at"`
	ConversationID  string         `json:"conversation_id"`
	InReplyToUserID string         `json:"in_reply_to_user_id"`
	Entities        *TweetEntities `json:"entities"`
	Attachments     *struct {
		MediaKeys []string `json:"media_keys"`
	} `json:"attachments"`
	ReferencedTweets []struct {
		Type string `json:"type"` // replied_to | quoted | retweeted
		ID   string `json:"id"`
	} `json:"referenced_tweets"`
	NoteTweet *struct {
		Text     string         `json:"text"`
		Entities *TweetEntities `json:"entities"`
	} `json:"note_tweet"` // present for long-form (>280 char) posts
}

type TweetEntities struct {
	URLs []struct {
		URL         string `json:"url"` // t.co link as it appears in text
		ExpandedURL string `json:"expanded_url"`
		DisplayURL  string `json:"display_url"`
		UnwoundURL  string `json:"unwound_url"`
		MediaKey    string `json:"media_key"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Images      []struct {
			URL    string `json:"url"`
			Width  int64  `json:"width"`
			Height int64  `json:"height"`
		} `json:"images"` // link card preview images, when X has them
	} `json:"urls"`
}

func (t *Tweet) RepliedTo() string {
	for _, r := range t.ReferencedTweets {
		if r.Type == "replied_to" {
			return r.ID
		}
	}
	return ""
}

func (t *Tweet) IsRetweet() bool {
	for _, r := range t.ReferencedTweets {
		if r.Type == "retweeted" {
			return true
		}
	}
	return false
}

// FullText returns long-form text/entities when present.
func (t *Tweet) FullText() (string, *TweetEntities) {
	if t.NoteTweet != nil && t.NoteTweet.Text != "" {
		return t.NoteTweet.Text, t.NoteTweet.Entities
	}
	return t.Text, t.Entities
}

type XMedia struct {
	MediaKey        string `json:"media_key"`
	Type            string `json:"type"` // photo | video | animated_gif
	URL             string `json:"url"`
	PreviewImageURL string `json:"preview_image_url"`
	AltText         string `json:"alt_text"`
	Width           int64  `json:"width"`
	Height          int64  `json:"height"`
	DurationMS      int64  `json:"duration_ms"`
	Variants        []struct {
		BitRate     int64  `json:"bit_rate"`
		ContentType string `json:"content_type"`
		URL         string `json:"url"`
	} `json:"variants"`
}

type timelineResponse struct {
	Data     []Tweet `json:"data"`
	Includes struct {
		Media []XMedia `json:"media"`
	} `json:"includes"`
	Meta struct {
		NewestID    string `json:"newest_id"`
		ResultCount int    `json:"result_count"`
		NextToken   string `json:"next_token"`
	} `json:"meta"`
	Errors []json.RawMessage `json:"errors"`
}

// Timeline is a page of tweets (newest first, as X returns them) plus media lookup.
type Timeline struct {
	Tweets []Tweet
	Media  map[string]XMedia
}

type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("x api rate limited until %s", e.Reset.Format(time.RFC3339))
}

func (c *XClient) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	req.Header.Set("User-Agent", "oregon-state-football-feeder/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode == http.StatusTooManyRequests {
		reset := time.Now().Add(15 * time.Minute)
		if s := resp.Header.Get("x-rate-limit-reset"); s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				reset = time.Unix(n, 0)
			}
		}
		return &RateLimitError{Reset: reset}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("x api %s: %s: %s", path, resp.Status, truncate(string(body), 500))
	}
	return json.Unmarshal(body, out)
}

func (c *XClient) LookupUserID(ctx context.Context, username string) (string, error) {
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/users/by/username/"+url.PathEscape(username), nil, &out); err != nil {
		return "", err
	}
	if out.Data.ID == "" {
		return "", fmt.Errorf("x user %q not found", username)
	}
	return out.Data.ID, nil
}

// UserTweets returns tweets newer than sinceID (or the latest `limit` if sinceID is empty).
// Retweets are always excluded; replies are excluded unless includeReplies is set
// (needed to pick up self-reply threads).
func (c *XClient) UserTweets(ctx context.Context, userID, sinceID string, includeReplies bool, limit int) (*Timeline, error) {
	tl := &Timeline{Media: map[string]XMedia{}}
	exclude := "retweets"
	if !includeReplies {
		exclude = "retweets,replies"
	}

	pageToken := ""
	for page := 0; page < 5; page++ {
		q := url.Values{}
		q.Set("max_results", strconv.Itoa(max(5, min(limit, 100))))
		q.Set("exclude", exclude)
		q.Set("tweet.fields", "created_at,entities,attachments,referenced_tweets,in_reply_to_user_id,conversation_id,note_tweet")
		q.Set("expansions", "attachments.media_keys")
		q.Set("media.fields", "type,url,preview_image_url,alt_text,width,height,duration_ms,variants")
		if sinceID != "" {
			q.Set("since_id", sinceID)
		}
		if pageToken != "" {
			q.Set("pagination_token", pageToken)
		}

		var resp timelineResponse
		if err := c.get(ctx, "/users/"+userID+"/tweets", q, &resp); err != nil {
			return nil, err
		}
		tl.Tweets = append(tl.Tweets, resp.Data...)
		for _, m := range resp.Includes.Media {
			tl.Media[m.MediaKey] = m
		}

		// Without since_id we only want the latest page.
		if sinceID == "" || resp.Meta.NextToken == "" {
			break
		}
		pageToken = resp.Meta.NextToken
	}
	return tl, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
