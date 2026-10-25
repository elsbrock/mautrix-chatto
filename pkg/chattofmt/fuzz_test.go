package chattofmt

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/html"
	"maunium.net/go/mautrix/event"
)

func FuzzToMatrix(f *testing.F) {
	for _, seed := range []string{
		"", "**a**", "@alice hi", "```\ncode\n```", "<b>x</b>", "[a](javascript:alert(1))",
		"| a |\n|---|\n| b |", "> q\n@alice", "\\\\*x*", "www.example.com/@alice", "![x](y)", "[^1]\n\n[^1]: note",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		content := ToMatrix(context.Background(), body, testToMatrixOpts())
		if content.FormattedBody == "" {
			return
		}
		if !utf8.ValidString(body) {
			return
		}
		// The output must never contain script-capable markup.
		lower := strings.ToLower(content.FormattedBody)
		if strings.Contains(lower, "<script") || strings.Contains(lower, "javascript:") && strings.Contains(lower, "href=\"javascript:") {
			t.Fatalf("unsafe html %q for %q", content.FormattedBody, body)
		}
		if _, err := html.Parse(strings.NewReader(content.FormattedBody)); err != nil {
			t.Fatalf("unparsable html: %v", err)
		}
	})
}

func FuzzFromMatrix(f *testing.F) {
	for _, seed := range []string{
		"", "<b>x</b>", "<a href=\"https://matrix.to/#/@chatto_ualice:example.com\">A</a>", "<pre><code>```</code></pre>",
		"<ol start=\"5\"><li>x</li></ol>", "<table><tr><td>x</td></tr></table>", "<span data-mx-spoiler>s</span>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, formatted string) {
		content := &event.MessageEventContent{MsgType: event.MsgText, Body: "fallback", Format: event.FormatHTML, FormattedBody: formatted}
		_ = FromMatrix(context.Background(), content, testFromMatrixOpts())
	})
}
