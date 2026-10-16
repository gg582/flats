package doccheck

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// check writes files under a temporary root and audits them.
func check(t *testing.T, files map[string]string) *Report {
	t.Helper()
	root := t.TempDir()
	var list []string
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, name)
	}
	sort.Strings(list)
	r, err := Check(root, list)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func errorStrings(r *Report) []string {
	var out []string
	for _, f := range r.Errors {
		out = append(out, f.String())
	}
	return out
}

func TestErrors(t *testing.T) {
	guide := "# Set up Flats\n\n## Use `flats serve`\n\n<a id=\"custom-id\"></a>\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "valid links",
			files: map[string]string{
				"README.md": "# Flats\n\n[docs](docs/) [guide](docs/guide.md#set-up-flats) [code](docs/guide.md#use-flats-serve)\n" +
					"[ref][r] [self](#flats) [reload]() [root](/docs/guide.md) [pct](docs/a%20b.md) [web](https://example.com)\n" +
					"![logo](assets/logo.png) <a href=\"docs/guide.md#custom-id\">html</a>\n\n[r]: docs/guide.md\n",
				"docs/README.md":  "# Docs\n",
				"docs/guide.md":   guide,
				"docs/a b.md":     "",
				"assets/logo.png": "",
			},
		},
		{
			name: "missing file",
			files: map[string]string{
				"README.md": "[x](nope.md)\n\n<div>\n<img src=\"missing.png\">\n</div>\n",
			},
			want: []string{
				`README.md:1: broken link "nope.md": nope.md is not in the repository`,
				`README.md:4: broken link "missing.png": missing.png is not in the repository`,
			},
		},
		{
			name: "link split across lines",
			files: map[string]string{
				"README.md": "[a split\nlink](nope.md)\n",
			},
			want: []string{`README.md:1: broken link "nope.md": nope.md is not in the repository`},
		},
		{
			name: "wrong case",
			files: map[string]string{
				"README.md":     "[x](docs/Guide.md)\n",
				"docs/guide.md": guide,
			},
			want: []string{`README.md:1: broken link "docs/Guide.md": docs/Guide.md is not in the repository (did you mean docs/guide.md?)`},
		},
		{
			name:  "outside the repository",
			files: map[string]string{"docs/README.md": "[x](../../etc/passwd)\n"},
			want:  []string{`docs/README.md:1: broken link "../../etc/passwd": ../etc/passwd is not in the repository`},
		},
		{
			name: "missing anchor",
			files: map[string]string{
				"README.md":     "[x](docs/guide.md#no-such-heading) [y](#nope)\n",
				"docs/guide.md": guide,
			},
			want: []string{
				`README.md:1: broken anchor "docs/guide.md#no-such-heading" (#no-such-heading is not a heading or id in docs/guide.md)`,
				`README.md:1: broken anchor "#nope" (#nope is not a heading or id in README.md)`,
			},
		},
		{
			name: "ignored in code and comments",
			files: map[string]string{
				"README.md": "```\n[x](fenced.md)\n```\n\n`[x](inline.md)`\n\n<!-- [x](comment.md) -->\n",
			},
		},
		{
			name: "relative links in docs/agent",
			files: map[string]string{
				"README.md":                    "[t](docs/agent/topics/example.md)\n",
				"docs/agent/topics/example.md": "# Example\n\n[a](other.md) [b](example.md#example) [c](#example) [d]() [e](https://example.com)\n",
				"docs/agent/topics/other.md":   "# Other\n",
			},
			want: []string{
				`docs/agent/topics/example.md:3: relative link "other.md" in docs/agent/ breaks when served through MCP or llms.txt`,
				`docs/agent/topics/example.md:3: relative link "example.md#example" in docs/agent/ breaks when served through MCP or llms.txt`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorStrings(check(t, tc.files)); !slices.Equal(got, tc.want) {
				t.Errorf("errors:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestHeadingAnchors(t *testing.T) {
	for _, tc := range []struct{ name, doc, anchor string }{
		{"words", "# Set up Flats", "set-up-flats"},
		{"punctuation", "# API & CLI", "api--cli"},
		{"code span", "# Use `flats serve`", "use-flats-serve"},
		{"link label", "# [Linked](README.md) title", "linked-title"},
		{"emphasis", "# **Bold** _it_", "bold-it"},
		{"non-Latin", "# 한국어 제목", "한국어-제목"},
		{"underscore and dot", "# snake_case v1.2", "snake_case-v12"},
		{"closing hashes", "# Trailing #", "trailing"},
		{"setext", "Setext\n------", "setext"},
		{"autolink", "# See <https://example.com>", "see-httpsexamplecom"},
		{"bare URL", "# See https://example.com", "see-httpsexamplecom"},
		{"duplicates", "# Setup\n\n## Setup\n\nSetup\n-----", "setup-2"},
		{"explicit anchor does not shift numbering", "<a name=\"setup\"></a>\n\n# Setup\n\n## Setup", "setup-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(t, map[string]string{"README.md": tc.doc + "\n\n[x](#" + tc.anchor + ")\n"})
			if len(r.Errors) != 0 {
				t.Errorf("errors = %v", r.Errors)
			}
		})
	}
	// Two headings and an explicit anchor named like the first give two
	// generated slugs, not three.
	r := check(t, map[string]string{"README.md": "<a name=\"setup\"></a>\n\n# Setup\n\n## Setup\n\n[x](#setup-2)\n"})
	if len(r.Errors) != 1 {
		t.Errorf("errors = %v, want a broken #setup-2", r.Errors)
	}
}

func TestReachability(t *testing.T) {
	r := check(t, map[string]string{
		"README.md":                    "[docs](docs) [example](examples/demo/main.js)\n",
		"docs/README.md":               "[a](a.md)\n",
		"docs/a.md":                    "# A\n",
		"docs/orphan.md":               "# Orphan\n",
		"docs/embedded/topic.md":       "# Embedded\n",
		"docs/embedded/_hidden.md":     "# Hidden\n",
		"docs/embedded/sub/_skip/x.md": "# Skipped\n",
		"docs/embed.go":                "package docs\n\n//go:embed embedded\nvar fs embed.FS\n",
		"docs/named.md":                "# Named\n",
		"docs/loader.go":               "package docs\n\nconst p = \"named.md\"\n",
		"docs/tested.md":               "# Tested\n",
		"docs/loader_test.go":          "package docs\n\nconst q = \"tested.md\"\n",
		"examples/demo/main.js":        "",
		"examples/demo/README.md":      "# Demo\n",
		"internal/pkg/README.md":       "# Package notes\n",
		".cursor/skills/x/SKILL.md":    "[skills are not checked](nope.md)\n",
	})
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v", r.Errors)
	}
	if want := []string{"docs/embedded/_hidden.md", "docs/embedded/sub/_skip/x.md", "docs/orphan.md"}; !slices.Equal(r.Orphans, want) {
		t.Errorf("orphans = %q, want %q", r.Orphans, want)
	}
	want := []Colocated{{Path: "docs/tested.md", TestOnly: true}, {Path: "internal/pkg/README.md"}}
	if !slices.Equal(r.Colocated, want) {
		t.Errorf("colocated = %v, want %v", r.Colocated, want)
	}
}

func TestHashLiterals(t *testing.T) {
	r := check(t, map[string]string{
		"README.md": "Commit `0123456789abcdef0123456789abcdef01234567`.\n\n```\n" +
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n```\n\nShort 0123456 and 41 chars 0123456789abcdef0123456789abcdef012345678.\n",
	})
	if len(r.Hashes) != 1 || r.Hashes[0].Line != 1 {
		t.Errorf("hashes = %v, want only line 1", r.Hashes)
	}
}

func TestEmbedPatterns(t *testing.T) {
	got := embedPatterns("a.md\tb/*.md \"c d.md\" `e.md` all:f")
	if want := []string{"a.md", "b/*.md", "c d.md", "e.md", "all:f"}; !slices.Equal(got, want) {
		t.Errorf("embedPatterns = %q, want %q", got, want)
	}
}
