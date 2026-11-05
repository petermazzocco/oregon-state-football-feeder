package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func ents(t *testing.T, js string) *TweetEntities {
	t.Helper()
	var e TweetEntities
	if err := json.Unmarshal([]byte(js), &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func facetText(rt RichText, i int) string {
	f := rt.Facets[i]
	return rt.Text[f.Index.ByteStart:f.Index.ByteEnd]
}

func TestBuildRichText_LinksMediaEntitiesTags(t *testing.T) {
	text := "Game day 🦫 &amp; we're ready! #GoBeavs @OregonState tix: https://t.co/abc123 https://t.co/media99"
	e := ents(t, `{"urls":[
		{"url":"https://t.co/abc123","expanded_url":"https://osubeavers.com/tickets","display_url":"osubeavers.com/tickets"},
		{"url":"https://t.co/media99","expanded_url":"https://x.com/BeaverFootball/status/1/photo/1","display_url":"pic.x.com/xyz","media_key":"3_1"}
	]}`)

	rt := BuildRichText(text, e, "https://x.com/BeaverFootball/status/1")

	want := "Game day 🦫 & we're ready! #GoBeavs @OregonState tix: osubeavers.com/tickets"
	if rt.Text != want {
		t.Fatalf("text:\n got %q\nwant %q", rt.Text, want)
	}
	if len(rt.Facets) != 3 {
		t.Fatalf("want 3 facets, got %d", len(rt.Facets))
	}
	got := map[string]string{}
	for i, f := range rt.Facets {
		feat := f.Features[0]
		switch {
		case feat.RichtextFacet_Link != nil:
			got[facetText(rt, i)] = feat.RichtextFacet_Link.Uri
		case feat.RichtextFacet_Tag != nil:
			got[facetText(rt, i)] = "tag:" + feat.RichtextFacet_Tag.Tag
		}
	}
	for k, v := range map[string]string{
		"osubeavers.com/tickets": "https://osubeavers.com/tickets",
		"#GoBeavs":               "tag:GoBeavs",
		"@OregonState":           "https://x.com/OregonState",
	} {
		if got[k] != v {
			t.Errorf("facet %q = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestBuildRichText_Truncates(t *testing.T) {
	long := strings.Repeat("Beavers win big again tonight in Corvallis. ", 12) // ~528 chars
	rt := BuildRichText(long+"https://t.co/zzz", ents(t, `{"urls":[{"url":"https://t.co/zzz","expanded_url":"https://example.com/a","display_url":"example.com/a"}]}`), "https://x.com/BeaverFootball/status/42")

	if n := uniseg.GraphemeClusterCount(rt.Text); n > maxGraphemes {
		t.Fatalf("too long: %d graphemes", n)
	}
	if !strings.HasSuffix(rt.Text, "Full post on X") {
		t.Fatalf("missing suffix: %q", rt.Text)
	}
	last := rt.Facets[len(rt.Facets)-1]
	if last.Features[0].RichtextFacet_Link.Uri != "https://x.com/BeaverFootball/status/42" {
		t.Fatalf("last facet should link original tweet")
	}
	for i, f := range rt.Facets {
		if int(f.Index.ByteEnd) > len(rt.Text) {
			t.Fatalf("facet %d out of range", i)
		}
	}
}

func TestBuildRichText_NoFalseMentionOrTag(t *testing.T) {
	rt := BuildRichText("email me@example.com, item&#1 and url x.com/a#frag", nil, "")
	if len(rt.Facets) != 0 {
		t.Fatalf("expected no facets, got %d: first=%q", len(rt.Facets), facetText(rt, 0))
	}
}
