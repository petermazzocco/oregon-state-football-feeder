package main

import (
	"html"
	"regexp"
	"strings"

	bsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/rivo/uniseg"
)

// Bluesky post text limit is 300 graphemes.
const maxGraphemes = 300

var (
	tcoRe     = regexp.MustCompile(`https://t\.co/[A-Za-z0-9]+`)
	hashtagRe = regexp.MustCompile(`(^|[^\p{L}\p{N}_&/])#([\p{L}\p{N}_]*\p{L}[\p{L}\p{N}_]*)`)
	mentionRe = regexp.MustCompile(`(^|[^\p{L}\p{N}_@])@([A-Za-z0-9_]{1,15})\b`)
)

// segment is a run of post text, optionally linked. Linked segments are atomic
// when truncating.
type segment struct {
	text string
	link string
}

// RichText is the final Bluesky text with byte-indexed facets.
type RichText struct {
	Text   string
	Facets []*bsky.RichtextFacet
}

// BuildRichText converts a tweet into Bluesky text + facets:
//   - t.co links become their real URLs (shown as display_url, linked to the full URL)
//   - t.co links that point at attached media are dropped (media is embedded instead)
//   - HTML entities (&amp; etc.) are decoded
//   - #hashtags become tag facets, @mentions link to the X profile
//   - if it doesn't fit in 300 graphemes it's truncated with a link to the original
func BuildRichText(text string, ents *TweetEntities, tweetURL string) RichText {
	byTco := map[string]int{}
	if ents != nil {
		for i, u := range ents.URLs {
			byTco[u.URL] = i
		}
	}

	var segs []segment
	last := 0
	for _, loc := range tcoRe.FindAllStringIndex(text, -1) {
		segs = append(segs, segment{text: html.UnescapeString(text[last:loc[0]])})
		last = loc[1]
		tco := text[loc[0]:loc[1]]
		i, ok := byTco[tco]
		if !ok {
			segs = append(segs, segment{text: tco, link: tco})
			continue
		}
		u := ents.URLs[i]
		if u.MediaKey != "" || isMediaPermalink(u.ExpandedURL) {
			continue // media is attached as an embed
		}
		full := firstNonEmpty(u.UnwoundURL, u.ExpandedURL, u.URL)
		display := firstNonEmpty(u.DisplayURL, full)
		segs = append(segs, segment{text: display, link: full})
	}
	segs = append(segs, segment{text: html.UnescapeString(text[last:])})

	segs = trimSegments(segs)
	if countGraphemes(segs) > maxGraphemes {
		segs = truncateSegments(segs, tweetURL)
	}
	return render(segs)
}

func render(segs []segment) RichText {
	var b strings.Builder
	var facets []*bsky.RichtextFacet
	for _, s := range segs {
		start := b.Len()
		b.WriteString(s.text)
		if s.link != "" {
			facets = append(facets, linkFacet(start, b.Len(), s.link))
		} else {
			facets = append(facets, plainFacets(s.text, start)...)
		}
	}
	return RichText{Text: b.String(), Facets: facets}
}

// plainFacets finds hashtags and mentions inside an unlinked text run.
func plainFacets(s string, offset int) []*bsky.RichtextFacet {
	var out []*bsky.RichtextFacet
	for _, m := range hashtagRe.FindAllStringSubmatchIndex(s, -1) {
		// m[4]:m[5] is the tag body; include the '#' just before it.
		tag := s[m[4]:m[5]]
		if len(tag) > 64 {
			continue
		}
		out = append(out, &bsky.RichtextFacet{
			Index: &bsky.RichtextFacet_ByteSlice{ByteStart: int64(offset + m[4] - 1), ByteEnd: int64(offset + m[5])},
			Features: []*bsky.RichtextFacet_Features_Elem{{
				RichtextFacet_Tag: &bsky.RichtextFacet_Tag{Tag: tag},
			}},
		})
	}
	for _, m := range mentionRe.FindAllStringSubmatchIndex(s, -1) {
		user := s[m[4]:m[5]]
		out = append(out, linkFacet(offset+m[4]-1, offset+m[5], "https://x.com/"+user))
	}
	return out
}

func linkFacet(start, end int, uri string) *bsky.RichtextFacet {
	return &bsky.RichtextFacet{
		Index: &bsky.RichtextFacet_ByteSlice{ByteStart: int64(start), ByteEnd: int64(end)},
		Features: []*bsky.RichtextFacet_Features_Elem{{
			RichtextFacet_Link: &bsky.RichtextFacet_Link{Uri: uri},
		}},
	}
}

// trimSegments removes leading/trailing whitespace from the post as a whole.
func trimSegments(segs []segment) []segment {
	for len(segs) > 0 && segs[0].link == "" {
		segs[0].text = strings.TrimLeft(segs[0].text, " \t\n")
		if segs[0].text != "" {
			break
		}
		segs = segs[1:]
	}
	for len(segs) > 0 && segs[len(segs)-1].link == "" {
		i := len(segs) - 1
		segs[i].text = strings.TrimRight(segs[i].text, " \t\n")
		if segs[i].text != "" {
			break
		}
		segs = segs[:i]
	}
	return segs
}

func countGraphemes(segs []segment) int {
	n := 0
	for _, s := range segs {
		n += uniseg.GraphemeClusterCount(s.text)
	}
	return n
}

// truncateSegments cuts the text to fit and appends "… <link to original>".
func truncateSegments(segs []segment, tweetURL string) []segment {
	suffix := []segment{{text: "…\n\n"}, {text: "Full post on X", link: tweetURL}}
	budget := maxGraphemes - countGraphemes(suffix)

	var out []segment
	used := 0
	for _, s := range segs {
		n := uniseg.GraphemeClusterCount(s.text)
		if used+n <= budget {
			out = append(out, s)
			used += n
			continue
		}
		if s.link == "" { // partially include plain text, preferring a word boundary
			out = append(out, segment{text: cutGraphemes(s.text, budget-used)})
		}
		break
	}
	out = trimSegments(out)
	return append(out, suffix...)
}

func cutGraphemes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	g := uniseg.NewGraphemes(s)
	end := 0
	for i := 0; i < n && g.Next(); i++ {
		_, end = g.Positions()
	}
	cut := s[:end]
	if end < len(s) {
		if i := strings.LastIndexAny(cut, " \n"); i > len(cut)/2 {
			cut = cut[:i]
		}
	}
	return cut
}

// isMediaPermalink catches t.co links to x.com/<user>/status/<id>/photo/1 etc.
func isMediaPermalink(u string) bool {
	return strings.Contains(u, "/photo/") || strings.Contains(u, "/video/")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
