package chattofmt

import (
	"context"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func testToMatrixOpts() *ToMatrixOptions {
	return &ToMatrixOptions{
		ServerURL: "https://chat.example.com",
		ResolveMention: func(_ context.Context, handle string) (MentionTarget, bool) {
			switch strings.ToLower(handle) {
			case "alice":
				return MentionTarget{UserID: "@chatto_ualice:example.com", DisplayName: "Alice"}, true
			case "first_last":
				return MentionTarget{UserID: "@chatto_ufl:example.com", DisplayName: "First Last"}, true
			}
			return MentionTarget{}, false
		},
	}
}

func TestToMatrix(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantHTML string // empty means no formatted body
		wantBody string
		mentions []id.UserID
	}{
		{name: "plain", in: "hello world", wantBody: "hello world"},
		{name: "empty", in: "", wantBody: ""},
		{name: "bold", in: "**bold** text", wantHTML: "<strong>bold</strong> text"},
		{name: "italic underscore", in: "_it_", wantHTML: "<em>it</em>"},
		{name: "intraword underscore stays", in: "snake_case_name", wantBody: "snake_case_name"},
		{name: "strike", in: "~~gone~~", wantHTML: "<del>gone</del>"},
		{name: "inline code", in: "use `x := 1`", wantHTML: "use <code>x := 1</code>"},
		{name: "code block lang", in: "```Go extra\nfmt.Println(\"<hi>\")\n```", wantHTML: `<pre><code class="language-go">fmt.Println(&#34;&lt;hi&gt;&#34;)` + "\n</code></pre>"},
		{name: "code keeps backslash", in: "`a\\b`", wantHTML: `<code>a\b</code>`},
		{name: "backslash is literal", in: `\*not\* *em*`, wantHTML: `\<em>not\</em> <em>em</em>`},
		{name: "newline is break", in: "line1\nline2", wantBody: "line1\nline2"},
		{name: "two paragraphs", in: "a\n\nb", wantBody: "a\n\nb"},
		{name: "newline with formatting", in: "**a**\nb", wantHTML: "<strong>a</strong><br>b"},
		{name: "quote", in: "> quoted", wantHTML: "<blockquote><p>quoted</p></blockquote>"},
		{name: "list", in: "- a\n- b", wantHTML: "<ul><li>a</li><li>b</li></ul>"},
		{name: "ordered list start", in: "3. c\n4. d", wantHTML: `<ol start="3"><li>c</li><li>d</li></ol>`},
		{name: "heading", in: "# Title", wantHTML: "<h1>Title</h1>"},
		{name: "setext disabled", in: "Title\n===", wantBody: "Title\n==="},
		{name: "hr disabled", in: "---", wantBody: "---"},
		{name: "raw html is text", in: "<b>x</b>", wantBody: "<b>x</b>"},
		{name: "link", in: "[site](https://example.com)", wantHTML: `<a href="https://example.com">site</a>`},
		{name: "unsupported scheme link is label", in: "[site](ftp://example.com)", wantHTML: "site", wantBody: "site"},
		{name: "root relative link", in: "[room](/chat/-/R1)", wantHTML: `<a href="https://chat.example.com/chat/-/R1">room</a>`},
		{name: "linkify", in: "see https://example.com/x ok", wantHTML: `see <a href="https://example.com/x">https://example.com/x</a> ok`},
		{name: "linkify www", in: "www.example.com", wantHTML: `<a href="https://www.example.com">www.example.com</a>`},
		{name: "bare domain not linked", in: "example.com", wantBody: "example.com"},
		{name: "image renders as bang link", in: "![alt](https://x.org/a.png)", wantHTML: `!<a href="https://x.org/a.png">alt</a>`},
		{
			name: "mention", in: "hi @alice!", wantHTML: `hi <a href="https://matrix.to/#/@chatto_ualice:example.com">Alice</a>!`,
			mentions: []id.UserID{"@chatto_ualice:example.com"},
		},
		{
			name: "mention with underscore", in: "@first_last", wantHTML: `<a href="https://matrix.to/#/@chatto_ufl:example.com">First Last</a>`,
			mentions: []id.UserID{"@chatto_ufl:example.com"},
		},
		{name: "unknown mention plain", in: "hi @bob", wantBody: "hi @bob"},
		{name: "mention in code ignored", in: "`@alice`", wantHTML: "<code>@alice</code>"},
		{name: "email not mention", in: "mail me@alice.com", wantBody: "mail me@alice.com"},
		{name: "table", in: "| a | b |\n|---|---|\n| 1 | 2 |", wantHTML: "<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>"},
		{name: "unicode", in: "héllo 🎉 **wörld**", wantHTML: "héllo 🎉 <strong>wörld</strong>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToMatrix(context.Background(), tt.in, testToMatrixOpts())
			if got.MsgType != event.MsgText {
				t.Fatalf("msgtype %q", got.MsgType)
			}
			if tt.wantHTML == "" {
				if got.FormattedBody != "" {
					t.Fatalf("unexpected formatted body %q", got.FormattedBody)
				}
				if got.Body != tt.wantBody {
					t.Fatalf("body = %q, want %q", got.Body, tt.wantBody)
				}
			} else {
				if got.Format != event.FormatHTML || got.FormattedBody != tt.wantHTML {
					t.Fatalf("formatted body = %q, want %q", got.FormattedBody, tt.wantHTML)
				}
				if tt.wantBody != "" && got.Body != tt.wantBody {
					t.Fatalf("body = %q, want %q", got.Body, tt.wantBody)
				}
			}
			var gotMentions []id.UserID
			if got.Mentions != nil {
				gotMentions = got.Mentions.UserIDs
			}
			if len(gotMentions) != len(tt.mentions) {
				t.Fatalf("mentions = %v, want %v", gotMentions, tt.mentions)
			}
			for i := range gotMentions {
				if gotMentions[i] != tt.mentions[i] {
					t.Fatalf("mentions = %v, want %v", gotMentions, tt.mentions)
				}
			}
		})
	}
}
