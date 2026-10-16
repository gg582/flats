package doccheck

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	nethtml "golang.org/x/net/html"
)

// hashRE matches full Git object names and SHA-256 digests.
var hashRE = regexp.MustCompile(`\b(?:[0-9a-fA-F]{64}|[0-9a-fA-F]{40})\b`)

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// rawLink is a link destination as written in a Markdown file.
type rawLink struct {
	line int
	dest string
}

// document is what the checker needs from one Markdown file.
type document struct {
	anchors    map[string]bool
	links      []rawLink
	hashLines  []int
	lineStarts []int
}

// parseDocument parses Markdown with a CommonMark/GFM parser and collects
// Markdown heading anchors, HTML id/name anchors, link and image
// destinations (reference links included) and raw HTML href/src, and the
// lines outside code blocks that hold hash literals.
//
// It deliberately skips what these docs do not use: anchors of raw HTML
// headings, and entity or backslash-escape decoding in headings and
// destinations. A doc that needs them fails with a false broken link.
func parseDocument(src []byte) *document {
	d := &document{anchors: map[string]bool{}, lineStarts: lineStarts(src)}
	root := md.Parser().Parse(text.NewReader(src))

	// Heading slugs are numbered among headings only: an explicit
	// <a name> does not shift them.
	slugCounts, headingSlugs := map[string]int{}, map[string]bool{}
	var codeLines [][2]int
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			if base := githubSlug(headingText(n, src)); base != "" {
				slug := uniqueSlug(base, slugCounts, headingSlugs)
				headingSlugs[slug] = true
				d.anchors[slug] = true
			}
		case *ast.Link:
			d.links = append(d.links, rawLink{line: d.line(nodeStart(n)), dest: string(n.Destination)})
		case *ast.Image:
			d.links = append(d.links, rawLink{line: d.line(nodeStart(n)), dest: string(n.Destination)})
		case *ast.RawHTML:
			var b bytes.Buffer
			for i := 0; i < n.Segments.Len(); i++ {
				seg := n.Segments.At(i)
				b.Write(seg.Value(src))
			}
			d.scanHTML(b.Bytes(), d.line(segmentStart(n.Segments)))
		case *ast.HTMLBlock:
			var b bytes.Buffer
			for i := 0; i < n.Lines().Len(); i++ {
				seg := n.Lines().At(i)
				b.Write(seg.Value(src))
			}
			if n.HasClosure() {
				b.Write(n.ClosureLine.Value(src))
			}
			d.scanHTML(b.Bytes(), d.line(segmentStart(n.Lines())))
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if lines := n.Lines(); lines.Len() > 0 {
				codeLines = append(codeLines, [2]int{
					d.line(lines.At(0).Start),
					d.line(lines.At(lines.Len() - 1).Start),
				})
			}
		}
		return ast.WalkContinue, nil
	})

	for i, line := range bytes.Split(src, []byte("\n")) {
		n := i + 1
		if !inRanges(n, codeLines) && hashRE.Match(line) {
			d.hashLines = append(d.hashLines, n)
		}
	}
	sort.SliceStable(d.links, func(i, j int) bool { return d.links[i].line < d.links[j].line })
	return d
}

// scanHTML records id/name anchors and a/img/link/script/source
// destinations in raw HTML that starts at line. Comments, including
// PROTECTED blocks, are skipped by the tokenizer.
func (d *document) scanHTML(b []byte, line int) {
	z := nethtml.NewTokenizer(bytes.NewReader(b))
	offset := 0
	for {
		tt := z.Next()
		tokenLine := line + bytes.Count(b[:offset], []byte("\n"))
		offset += len(z.Raw())
		switch tt {
		case nethtml.ErrorToken:
			return
		case nethtml.StartTagToken, nethtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				key, val := string(k), strings.TrimSpace(string(v))
				if val == "" {
					continue
				}
				if key == "id" || (key == "name" && tag == "a") {
					d.anchors[val] = true
				}
				if (key == "href" || key == "src") && (tag == "a" || tag == "img" || tag == "link" || tag == "script" || tag == "source") {
					d.links = append(d.links, rawLink{line: tokenLine, dest: val})
				}
			}
		}
	}
}

func (d *document) line(offset int) int {
	if offset < 0 {
		return 0
	}
	return sort.SearchInts(d.lineStarts, offset+1)
}

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func segmentStart(s *text.Segments) int {
	if s == nil || s.Len() == 0 {
		return -1
	}
	return s.At(0).Start
}

// nodeStart returns the source offset of the first text inside an inline
// node, falling back to the start of its enclosing block.
func nodeStart(n ast.Node) int {
	start := -1
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			start = c.Segment.Start
			return ast.WalkStop, nil
		case *ast.RawHTML:
			start = segmentStart(c.Segments)
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	if start >= 0 {
		return start
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == ast.TypeBlock && p.Lines().Len() > 0 {
			return p.Lines().At(0).Start
		}
	}
	return -1
}

func inRanges(n int, ranges [][2]int) bool {
	for _, r := range ranges {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

// headingText returns a heading's text as written: code span contents and
// link labels are kept, markup and raw HTML tags are not.
func headingText(h *ast.Heading, src []byte) string {
	var b strings.Builder
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Text:
				b.Write(c.Segment.Value(src))
				if c.SoftLineBreak() || c.HardLineBreak() {
					b.WriteByte(' ')
				}
			case *ast.String:
				b.Write(c.Value)
			case *ast.AutoLink:
				// Its text is not a child node.
				b.Write(c.Label(src))
			case *ast.RawHTML, *ast.Image:
			default:
				walk(c)
			}
		}
	}
	walk(h)
	return b.String()
}

// githubSlug converts heading text to the anchor GitHub generates: lower
// case, letters, marks, numbers, connector punctuation and hyphens kept,
// spaces turned into hyphens, everything else dropped.
func githubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-', unicode.IsLetter(r), unicode.IsNumber(r), unicode.Is(unicode.M, r), unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uniqueSlug appends -1, -2, ... to repeated slugs as GitHub does.
func uniqueSlug(base string, counts map[string]int, used map[string]bool) string {
	for {
		n := counts[base]
		counts[base] = n + 1
		slug := base
		if n > 0 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		if !used[slug] {
			return slug
		}
	}
}
