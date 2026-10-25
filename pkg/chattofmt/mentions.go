// Package chattofmt converts message bodies between Chatto's Markdown dialect
// and Matrix's HTML formatting.
//
// The mention and URL rules here are ported from Chatto's
// cli/internal/core/mentions.go and mention_urls.go (AGPL-3.0-or-later,
// © ChattoCorp GmbH) so that the bridge resolves exactly the mentions that
// Chatto's server notifies and its frontend renders. The shared upstream test
// cases live in testdata/mentions_extraction.json.
package chattofmt

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"mvdan.cc/xurls/v2"
)

// scanMentions finds the @handle mentions in source[start:stop] and calls add
// with each handle and the offset of its '@'. An '@' directly after a letter
// or digit (user@example.com) does not start a mention. A handle contains
// letters, digits, '_' and '-'; a '.' is allowed only between two handle
// characters.
func scanMentions(source []byte, start, stop int, add func(handle string, offset int)) {
	for at := start; at < stop; at++ {
		if source[at] != '@' || (at > 0 && isMentionAlphanumeric(source[at-1])) {
			continue
		}
		end := at + 1
		for end < stop && isMentionHandleChar(source[end]) {
			end++
		}
		if end == at+1 {
			continue
		}
		for end+1 < stop && source[end] == '.' && isMentionHandleChar(source[end+1]) {
			end += 2
			for end < stop && isMentionHandleChar(source[end]) {
				end++
			}
		}
		add(string(source[at+1:end]), at)
		at = end - 1
	}
}

func isMentionAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isMentionHandleChar(c byte) bool {
	return isMentionAlphanumeric(c) || c == '_' || c == '-'
}

// IsValidHandle reports whether login can be written as an @mention that
// Chatto will recognise in its entirety.
func IsValidHandle(login string) bool {
	if login == "" {
		return false
	}
	found := ""
	src := []byte("@" + login)
	scanMentions(src, 0, len(src), func(handle string, _ int) {
		if found == "" {
			found = handle
		}
	})
	return found == login
}

// markdownSource doubles every backslash. Chatto's renderer disables Markdown
// backslash escapes, but goldmark always honours them, so doubling makes
// goldmark read each original backslash as a literal character.
func markdownSource(body string) []byte {
	return []byte(strings.ReplaceAll(body, `\`, `\\`))
}

// undoubleBackslashes reverses markdownSource for raw (unescaped) output such
// as code spans and code blocks.
func undoubleBackslashes(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(`\\`), []byte(`\`))
}

// ExtractMentions returns the unique @handles in body in order of first
// appearance, ignoring handles inside code, blockquotes, links and URLs.
func ExtractMentions(body string) []string {
	if !strings.Contains(body, "@") {
		return nil
	}
	source := markdownSource(body)
	root := chattoMarkdown.Parser().Parse(text.NewReader(source))
	mergeAdjacentText(root)
	seen := make(map[string]struct{})
	var handles []string
	urls := newURLIndex(source)
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.Kind() {
		case ast.KindText:
			seg := node.(*ast.Text).Segment
			scanMentions(source, seg.Start, seg.Stop, func(handle string, offset int) {
				if urls.contains(offset) {
					return
				}
				if _, dup := seen[handle]; !dup {
					seen[handle] = struct{}{}
					handles = append(handles, handle)
				}
			})
		case ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindBlockquote, ast.KindCodeSpan,
			ast.KindLink, ast.KindAutoLink, ast.KindImage:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return handles
}

// mergeAdjacentText joins sibling text nodes whose segments touch. Goldmark
// splits text at unmatched delimiters such as '_', which would otherwise cut
// handles like @first_last in two.
func mergeAdjacentText(root ast.Node) {
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		cur, ok := node.(*ast.Text)
		if !ok {
			return ast.WalkContinue, nil
		}
		for {
			next, ok := cur.NextSibling().(*ast.Text)
			if !ok || cur.SoftLineBreak() || cur.HardLineBreak() || cur.IsRaw() || next.IsRaw() ||
				cur.Segment.Stop != next.Segment.Start || cur.Segment.Padding != 0 || next.Segment.Padding != 0 {
				break
			}
			cur.Segment = cur.Segment.WithStop(next.Segment.Stop)
			cur.SetSoftLineBreak(next.SoftLineBreak())
			cur.SetHardLineBreak(next.HardLineBreak())
			cur.Parent().RemoveChild(cur.Parent(), next)
		}
		return ast.WalkContinue, nil
	})
}

var (
	urlCandidateRegexp = xurls.Relaxed()
	// A scheme needs "//" after its colon, so example.com:8443 has none.
	// mailto: is the only linkified scheme without "//".
	urlSchemeRegexp  = regexp.MustCompile(`^(?i:([a-z][a-z0-9+.\-]*://)|mailto:)`)
	linkifiedSchemes = map[string]bool{"http://": true, "https://": true}
	linkifiedTLDs    = func() map[string]bool {
		tlds := make(map[string]bool, len(xurls.TLDs))
		for _, tld := range xurls.TLDs {
			tlds[strings.ToLower(tld)] = true
		}
		return tlds
	}()
)

type byteRange struct{ start, end int }

// urlIndex finds the URLs that Chatto's frontend linkifies, line by line.
type urlIndex struct {
	source []byte
	lines  map[int][]byteRange
}

func newURLIndex(source []byte) *urlIndex {
	return &urlIndex{source: source, lines: make(map[int][]byteRange)}
}

func (x *urlIndex) rangesForLine(offset int) []byteRange {
	lineStart := bytes.LastIndexByte(x.source[:offset], '\n') + 1
	ranges, scanned := x.lines[lineStart]
	if !scanned {
		lineEnd := len(x.source)
		if n := bytes.IndexByte(x.source[lineStart:], '\n'); n >= 0 {
			lineEnd = lineStart + n
		}
		ranges = linkifiedURLRanges(x.source, lineStart, lineEnd)
		x.lines[lineStart] = ranges
	}
	return ranges
}

func (x *urlIndex) contains(offset int) bool {
	for _, r := range x.rangesForLine(offset) {
		if offset >= r.start && offset < r.end {
			return true
		}
	}
	return false
}

// within returns the linkified URL ranges fully inside [start, stop).
func (x *urlIndex) within(start, stop int) []byteRange {
	var out []byteRange
	seenLines := make(map[int]bool)
	for off := start; off < stop; {
		lineStart := bytes.LastIndexByte(x.source[:off], '\n') + 1
		if !seenLines[lineStart] {
			seenLines[lineStart] = true
			for _, r := range x.rangesForLine(off) {
				if r.start >= start && r.end <= stop {
					out = append(out, r)
				}
			}
		}
		n := bytes.IndexByte(x.source[off:stop], '\n')
		if n < 0 {
			break
		}
		off += n + 1
	}
	return out
}

func linkifiedURLRanges(source []byte, start, end int) []byteRange {
	var ranges []byteRange
	for _, match := range urlCandidateRegexp.FindAllIndex(source[start:end], -1) {
		if isLinkifiedURL(source, start+match[0], start+match[1]) {
			ranges = append(ranges, byteRange{start: start + match[0], end: start + match[1]})
		}
	}
	return ranges
}

func isLinkifiedURL(source []byte, start, end int) bool {
	candidate := string(source[start:end])
	if scheme := urlSchemeRegexp.FindStringSubmatch(candidate); scheme != nil {
		return scheme[1] == "" || linkifiedSchemes[strings.ToLower(scheme[1])]
	}
	if !strings.HasPrefix(strings.ToLower(candidate), "www.") || !isLinkifyBoundary(source, start) {
		return false
	}
	return linkifiedTLDs[strings.ToLower(urlTopLevelDomain(candidate))]
}

func isLinkifyBoundary(source []byte, offset int) bool {
	if offset == 0 {
		return true
	}
	previous := source[offset-1]
	return !isMentionAlphanumeric(previous) && strings.IndexByte(".:/-_@", previous) < 0
}

func urlTopLevelDomain(candidate string) string {
	host := candidate
	if at := strings.LastIndexByte(host, '@'); at >= 0 && !strings.ContainsAny(host[:at], "/?#") {
		host = host[at+1:]
	}
	if cut := strings.IndexAny(host, "/?#:"); cut >= 0 {
		host = host[:cut]
	}
	return host[strings.LastIndexByte(host, '.')+1:]
}
