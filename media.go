package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxImageBytes = 1_000_000         // Bluesky image blob limit
	maxVideoBytes = 100 * 1024 * 1024 // Bluesky video limit
	maxVideoMS    = 3 * 60 * 1000     // Bluesky video duration limit (3 min)
)

var mediaHTTP = &http.Client{Timeout: 3 * time.Minute}

func download(ctx context.Context, u string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "oregon-state-football-feeder/1.0")
	resp, err := mediaHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("download %s: exceeds %d bytes", u, limit)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// photoURL asks X's CDN for the largest rendition as JPEG.
func photoURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("name", "large")
	q.Set("format", "jpg")
	u.RawQuery = q.Encode()
	return u.String()
}

// fitImage returns bytes under Bluesky's 1MB limit, re-encoding/downscaling if needed.
func fitImage(data []byte, mime string) ([]byte, string, int64, int64, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decode image: %w", err)
	}
	b := img.Bounds()
	w, h := int64(b.Dx()), int64(b.Dy())
	if len(data) <= maxImageBytes && (mime == "image/jpeg" || mime == "image/png") {
		return data, mime, w, h, nil
	}

	cur := img
	for _, maxDim := range []int{2000, 1600, 1200, 1000, 800} {
		if b.Dx() > maxDim || b.Dy() > maxDim {
			cur = resize(img, maxDim)
		}
		for _, q := range []int{90, 80, 70, 60} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: q}); err != nil {
				return nil, "", 0, 0, err
			}
			if buf.Len() <= maxImageBytes {
				cb := cur.Bounds()
				return buf.Bytes(), "image/jpeg", int64(cb.Dx()), int64(cb.Dy()), nil
			}
		}
	}
	return nil, "", 0, 0, fmt.Errorf("could not compress image under %d bytes", maxImageBytes)
}

func resize(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w >= h {
		h = h * maxDim / w
		w = maxDim
	} else {
		w = w * maxDim / h
		h = maxDim
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// videoCandidates returns mp4 variant URLs, best quality first, capped at ~2.5 Mbps
// (higher bitrates rarely matter after Bluesky transcodes and make uploads huge).
func videoCandidates(m XMedia) []string {
	type v struct {
		br  int64
		url string
	}
	var vs []v
	for _, x := range m.Variants {
		if x.ContentType == "video/mp4" {
			vs = append(vs, v{x.BitRate, x.URL})
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].br > vs[j].br })
	var out, over []string
	for _, x := range vs {
		if x.br > 2_500_000 {
			over = append(over, x.url)
		} else {
			out = append(out, x.url)
		}
	}
	return append(out, over...)
}
