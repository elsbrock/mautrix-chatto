package chattofmt

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// chattoMarkdown mirrors the block and inline syntax that Chatto's frontend
// renders with markdown-it: no setext headings, no thematic breaks, no link
// reference definitions, no raw HTML. Images are parsed so that they can be
// rendered the way markdown-it renders them with the image rule disabled.
var chattoMarkdown = goldmark.New(
	goldmark.WithParser(parser.NewParser(
		parser.WithBlockParsers(
			util.Prioritized(parser.NewListParser(), 300),
			util.Prioritized(parser.NewListItemParser(), 400),
			util.Prioritized(parser.NewCodeBlockParser(), 500),
			util.Prioritized(parser.NewATXHeadingParser(), 600),
			util.Prioritized(parser.NewFencedCodeBlockParser(), 700),
			util.Prioritized(parser.NewBlockquoteParser(), 800),
			util.Prioritized(parser.NewParagraphParser(), 1000),
		),
		parser.WithInlineParsers(
			util.Prioritized(parser.NewCodeSpanParser(), 100),
			util.Prioritized(parser.NewLinkParser(), 200),
			util.Prioritized(parser.NewAutoLinkParser(), 300),
			util.Prioritized(parser.NewRawHTMLParser(), 400),
			util.Prioritized(wordBoundaryEmphasisParser{}, 450),
			util.Prioritized(parser.NewEmphasisParser(), 500),
		),
	)),
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.Footnote,
	),
)

// MentionTarget is what a Chatto @handle resolves to on the Matrix side.
type MentionTarget struct {
	UserID      string // Matrix user ID, e.g. @chatto_U123:example.com
	DisplayName string
}

// htmlRenderer renders a parsed Chatto body as Matrix HTML. It is not safe for
// concurrent use; create one per conversion.
type htmlRenderer struct {
	source   []byte
	urls     *urlIndex
	opts     *ToMatrixOptions
	mentions map[string]MentionTarget // keyed by handle as written
	buf      bytes.Buffer
	// formatted becomes true once anything beyond plain paragraphs is emitted.
	formatted bool
}

func (r *htmlRenderer) write(s string) { r.buf.WriteString(s) }

func (r *htmlRenderer) writeEscaped(b []byte) { r.buf.WriteString(html.EscapeString(string(b))) }

// textValue unescapes a text segment the way goldmark's own renderer does
// (backslash escapes and entities), which undoes the backslash doubling.
func textValue(b []byte) []byte {
	return util.UnescapePunctuations(util.ResolveNumericReferences(util.ResolveEntityNames(b)))
}

func (r *htmlRenderer) render(doc ast.Node) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(n, entering)
	})
}

func hasAncestor(n ast.Node, kinds ...ast.NodeKind) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		for _, k := range kinds {
			if p.Kind() == k {
				return true
			}
		}
	}
	return false
}

var codeLanguageRegexp = regexp.MustCompile(`^[a-z0-9+#_.-]+`)

func (r *htmlRenderer) renderNode(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node := n.(type) {
	case *ast.Document:
	case *ast.Paragraph:
		// Tight list items hold their text directly in the item.
		if _, inTightList := node.Parent().(*ast.ListItem); inTightList && node.Parent().Parent().(*ast.List).IsTight {
			return ast.WalkContinue, nil
		}
		if entering {
			r.write("<p>")
		} else {
			r.write("</p>")
		}
	case *ast.TextBlock:
		if !entering && node.NextSibling() != nil {
			r.write("<br>")
		}
	case *ast.Heading:
		r.formatted = true
		if entering {
			r.write(fmt.Sprintf("<h%d>", node.Level))
		} else {
			r.write(fmt.Sprintf("</h%d>", node.Level))
		}
	case *ast.Blockquote:
		r.formatted = true
		if entering {
			r.write("<blockquote>")
		} else {
			r.write("</blockquote>")
		}
	case *ast.List:
		r.formatted = true
		tag := "ul"
		if node.IsOrdered() {
			tag = "ol"
		}
		if entering {
			if node.IsOrdered() && node.Start != 1 {
				r.write(fmt.Sprintf(`<ol start="%d">`, node.Start))
			} else {
				r.write("<" + tag + ">")
			}
		} else {
			r.write("</" + tag + ">")
		}
	case *ast.ListItem:
		if entering {
			r.write("<li>")
		} else {
			r.write("</li>")
		}
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		if !entering {
			return ast.WalkContinue, nil
		}
		r.formatted = true
		lang := ""
		if fenced, ok := node.(*ast.FencedCodeBlock); ok && fenced.Info != nil {
			info := strings.ToLower(string(undoubleBackslashes(fenced.Info.Segment.Value(r.source))))
			lang = codeLanguageRegexp.FindString(info)
		}
		var code bytes.Buffer
		lines := node.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			code.Write(seg.Value(r.source))
		}
		if lang != "" {
			r.write(fmt.Sprintf(`<pre><code class="language-%s">`, html.EscapeString(lang)))
		} else {
			r.write("<pre><code>")
		}
		r.writeEscaped(undoubleBackslashes(code.Bytes()))
		r.write("</code></pre>")
		return ast.WalkSkipChildren, nil
	case *ast.HTMLBlock:
		// markdown-it with html:false shows raw HTML as literal text.
		if !entering {
			return ast.WalkContinue, nil
		}
		r.write("<p>")
		lines := node.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			val := bytes.TrimRight(seg.Value(r.source), "\n")
			if i > 0 {
				r.write("<br>")
			}
			r.writeEscaped(undoubleBackslashes(val))
		}
		if node.HasClosure() {
			r.write("<br>")
			r.writeEscaped(undoubleBackslashes(bytes.TrimRight(node.ClosureLine.Value(r.source), "\n")))
		}
		r.write("</p>")
		return ast.WalkSkipChildren, nil
	case *ast.RawHTML:
		if entering {
			for i := 0; i < node.Segments.Len(); i++ {
				seg := node.Segments.At(i)
				r.writeEscaped(undoubleBackslashes(seg.Value(r.source)))
			}
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeSpan:
		if !entering {
			return ast.WalkContinue, nil
		}
		r.formatted = true
		r.write("<code>")
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				r.writeEscaped(undoubleBackslashes(t.Segment.Value(r.source)))
			} else if s, ok := c.(*ast.String); ok {
				r.writeEscaped(undoubleBackslashes(s.Value))
			}
		}
		r.write("</code>")
		return ast.WalkSkipChildren, nil
	case *ast.Emphasis:
		r.formatted = true
		tag := "em"
		if node.Level == 2 {
			tag = "strong"
		}
		if entering {
			r.write("<" + tag + ">")
		} else {
			r.write("</" + tag + ">")
		}
	case *east.Strikethrough:
		r.formatted = true
		if entering {
			r.write("<del>")
		} else {
			r.write("</del>")
		}
	case *ast.Link:
		href, ok := r.allowedHref(string(textValue(node.Destination)))
		if !ok {
			// markdown-it renders links with unsupported protocols as their label.
			return ast.WalkContinue, nil
		}
		r.formatted = true
		if entering {
			r.write(`<a href="` + html.EscapeString(href) + `">`)
		} else {
			r.write("</a>")
		}
	case *ast.AutoLink:
		if !entering {
			return ast.WalkContinue, nil
		}
		label := string(textValue(node.Label(r.source)))
		dest := label
		if node.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(dest), "mailto:") {
			dest = "mailto:" + dest
		}
		if href, ok := r.allowedHref(dest); ok {
			r.formatted = true
			r.write(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + "</a>")
		} else {
			r.write(html.EscapeString("<" + label + ">"))
		}
		return ast.WalkSkipChildren, nil
	case *ast.Image:
		// markdown-it with the image rule disabled renders "!" and then the
		// link that follows it.
		if entering {
			r.write("!")
			if href, ok := r.allowedHref(string(textValue(node.Destination))); ok {
				r.formatted = true
				r.write(`<a href="` + html.EscapeString(href) + `">`)
			}
		} else if _, ok := r.allowedHref(string(textValue(node.Destination))); ok {
			r.write("</a>")
		}
	case *ast.Text:
		if !entering {
			return ast.WalkContinue, nil
		}
		r.renderText(node)
	case *ast.String:
		if entering {
			r.writeEscaped(node.Value)
		}
	case *east.Table:
		r.formatted = true
		if entering {
			r.write("<table>")
		} else {
			if node.LastChild() != nil && node.LastChild().Kind() == east.KindTableRow {
				r.write("</tbody>")
			}
			r.write("</table>")
		}
	case *east.TableHeader:
		if entering {
			r.write("<thead><tr>")
		} else {
			r.write("</tr></thead>")
			if node.NextSibling() != nil {
				r.write("<tbody>")
			}
		}
	case *east.TableRow:
		if entering {
			r.write("<tr>")
		} else {
			r.write("</tr>")
		}
	case *east.TableCell:
		tag := "td"
		if _, isHeader := node.Parent().(*east.TableHeader); isHeader {
			tag = "th"
		}
		if entering {
			r.write("<" + tag + ">")
		} else {
			r.write("</" + tag + ">")
		}
	case *east.FootnoteLink:
		if entering {
			r.formatted = true
			r.write(fmt.Sprintf("<sup>[%d]</sup>", node.Index))
		}
	case *east.FootnoteBacklink:
		return ast.WalkSkipChildren, nil
	case *east.FootnoteList:
		r.formatted = true
		if entering {
			r.write("<hr><ol>")
		} else {
			r.write("</ol>")
		}
	case *east.Footnote:
		if entering {
			r.write("<li>")
		} else {
			r.write("</li>")
		}
	default:
		// Unknown nodes render their children only.
	}
	return ast.WalkContinue, nil
}

// renderText writes a text run, turning linkified URLs into links and
// resolved @handles into Matrix pills, as Chatto's frontend does.
func (r *htmlRenderer) renderText(node *ast.Text) {
	seg := node.Segment
	inLink := hasAncestor(node, ast.KindLink, ast.KindAutoLink, ast.KindImage)
	inQuote := hasAncestor(node, ast.KindBlockquote)

	type span struct {
		start, end int
		kind       int // 1 = url, 2 = mention
		handle     string
	}
	var spans []span
	if !inLink {
		for _, u := range r.urls.within(seg.Start, seg.Stop) {
			spans = append(spans, span{start: u.start, end: u.end, kind: 1})
		}
		if !inQuote {
			scanMentions(r.source, seg.Start, seg.Stop, func(handle string, offset int) {
				if r.urls.contains(offset) {
					return
				}
				if _, ok := r.mentions[handle]; ok {
					spans = append(spans, span{start: offset, end: offset + 1 + len(handle), kind: 2, handle: handle})
				}
			})
		}
	}
	// URLs and mentions never overlap: mentions inside URLs are dropped above.
	sortSpans := func() {
		for i := 1; i < len(spans); i++ {
			for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
				spans[j], spans[j-1] = spans[j-1], spans[j]
			}
		}
	}
	sortSpans()

	pos := seg.Start
	for _, s := range spans {
		if s.start < pos {
			continue
		}
		r.writeEscaped(textValue(r.source[pos:s.start]))
		raw := string(textValue(r.source[s.start:s.end]))
		switch s.kind {
		case 1:
			href := raw
			if !urlSchemeRegexp.MatchString(href) {
				href = "https://" + href
			}
			r.formatted = true
			r.write(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(raw) + "</a>")
		case 2:
			target := r.mentions[s.handle]
			name := target.DisplayName
			if name == "" {
				name = s.handle
			}
			r.formatted = true
			r.write(`<a href="https://matrix.to/#/` + html.EscapeString(target.UserID) + `">` + html.EscapeString(name) + "</a>")
		}
		pos = s.end
	}
	r.writeEscaped(textValue(r.source[pos:seg.Stop]))
	if node.SoftLineBreak() || node.HardLineBreak() {
		// markdown-it is configured with breaks:true.
		r.write("<br>")
	}
}

// allowedHref applies Chatto's link policy: only http, https, mailto and
// root-relative paths are links. Root-relative paths are made absolute
// against the server URL when one is known.
func (r *htmlRenderer) allowedHref(dest string) (string, bool) {
	dest = strings.TrimSpace(dest)
	if strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, "//") {
		// Chatto's isAllowedMarkdownLink rejects backslashes and whitespace
		// in root-relative paths.
		if r.opts.ServerURL == "" || strings.ContainsAny(dest, "\\ \t\r\n") {
			return "", false
		}
		return strings.TrimRight(r.opts.ServerURL, "/") + dest, true
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// "http:evil" has no authority and is not a link on Chatto.
		if u.Host == "" {
			return "", false
		}
		return dest, true
	case "mailto":
		return dest, true
	}
	return "", false
}
