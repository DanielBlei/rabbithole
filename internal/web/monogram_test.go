// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"strings"
	"testing"
)

func TestMonogramLetter(t *testing.T) {
	cases := []struct {
		name, url, letter string
	}{
		{"Hugging Face blog", "https://huggingface.co/blog/feed.xml", "H"},
		{"  #42 news", "https://www.example.com/x", "4"},
		{"", "https://hnrss.org/x", "H"},
		{"", "", "?"},
	}
	for _, c := range cases {
		got := monogram(c.name, c.url, 20)
		if got.Letter != c.letter {
			t.Errorf("monogram(%q, %q).Letter = %q, want %q", c.name, c.url, got.Letter, c.letter)
		}
	}
}

func TestMonogramKey(t *testing.T) {
	cases := []struct {
		name, url, key string
	}{
		{"  #42 news", "https://www.example.com/x", "example.com"},
		{"Crossref", "https://api.crossref.org/works", "crossref"},
		{"", "", "?"},
	}
	for _, c := range cases {
		if got := monogramKey(c.name, c.url); got != c.key {
			t.Errorf("monogramKey(%q, %q) = %q, want %q", c.name, c.url, got, c.key)
		}
	}
}

// Fixed vectors, worked from the prototype's hash: h = h*31 + code, as a uint32,
// over the UTF-16 code units of the key, then h mod 8.
//
// For "hnrss.org" the full run is 104, 3334, 103468, 3207623, 99436428,
// 3082529314, 1069128333, 3078207365, 935147906, and 935147906 mod 8 is 2.
// Mod 8 alone it is shorter: 31 is 7 mod 8 and 2^32 is divisible by 8, so each
// step is h = 7h + c (mod 8). The codes mod 8 are h0 n6 r2 s3 s3 .6 o7 r2 g7,
// giving 0, 6, 4, 7, 4, 2, 5, 5, 2.
//
// The rabbit's two units are D83D and DC07 (5 and 7 mod 8): 0 -> 5 -> 2, and
// ".example" (6 5 0 1 5 0 4 5) takes it 4, 1, 7, 2, 3, 5, 7, 6. Hashing the one
// code point 1F407 (7 mod 8) instead gives 3, which is the mistake this guards
// against.
func TestMonogramColor(t *testing.T) {
	cases := []struct {
		name, url string
		want      int
	}{
		{"Hugging Face blog", "https://huggingface.co/blog/feed.xml", 0},
		{"  #42 news", "https://www.example.com/x", 5},
		{"", "https://hnrss.org/x", 2},
		{"Crossref", "https://api.crossref.org/works", 3},
		{"", "", 7},
		{"🐇.example", "", 6},
		{"日本語.jp", "", 5},
	}
	for _, c := range cases {
		got := monogram(c.name, c.url, 20).Color
		if got != c.want {
			t.Errorf("monogram(%q, %q).Color = %d, want %d", c.name, c.url, got, c.want)
		}
	}
}

// Hand check for "arxiv.org". 31 is 7 mod 8, and 2^32 is divisible by 8, so the
// uint32 wrap does not change the result mod 8 and each step is h = 7h + c (mod 8).
// Codes mod 8: a1 r2 x0 i1 v6 .6 o7 r2 g7. Running h: 1, 1, 7, 2, 4, 2, 5, 5, 2.
// Final index 2.
func TestMonogramColorArxiv(t *testing.T) {
	if got := colorIndex("arxiv.org"); got != 2 {
		t.Fatalf("colorIndex(\"arxiv.org\") = %d, want 2", got)
	}
	if got := monogram("arXiv", "https://arxiv.org/rss", 20).Color; got != 2 {
		t.Fatalf("monogram for arxiv.org Color = %d, want 2", got)
	}
}

func TestFaviconRender(t *testing.T) {
	tmpl := pageTmpl("feed.html")
	if _, err := tmpl.New("t").
		Parse(`{{template "favicon" (monogram "Alpha" "https://alpha.test/f" 20)}}`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "t", nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"fav--mono", ">A<", "--fs:20px"} {
		if !strings.Contains(out, want) {
			t.Errorf("favicon render missing %q in %s", want, out)
		}
	}
}
