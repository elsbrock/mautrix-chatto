package chattofmt

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func testFromMatrixOpts() *FromMatrixOptions {
	return &FromMatrixOptions{
		ResolveUser: func(_ context.Context, userID id.UserID) (string, bool) {
			switch userID {
			case "@chatto_ualice:example.com":
				return "alice", true
			case "@bob:matrix.org":
				return "bob", true
			case "@weird:example.com":
				return "not a handle", true
			}
			return "", false
		},
		ResolveEventLink: func(_ context.Context, roomID id.RoomID, eventID id.EventID) (string, bool) {
			if eventID == "$known" {
				return "https://chat.example.com/chat/-/R1/m/E1", true
			}
			return "", false
		},
	}
}

func TestFromMatrix(t *testing.T) {
	tests := []struct {
		name string
		body string
		html string
		want string
	}{
		{name: "plain body", body: "hello *raw*", want: "hello *raw*"},
		{name: "bold", html: "<strong>bold</strong> x", want: "**bold** x"},
		{name: "bold between words", html: "a <b>b</b> c", want: "a **b** c"},
		{name: "italic", html: "<em>it</em>", want: "*it*"},
		{name: "strike", html: "<del>x</del>", want: "~~x~~"},
		{name: "underline dropped", html: "<u>u</u>", want: "u"},
		{name: "spoiler kept", html: `<span data-mx-spoiler="">secret</span>`, want: "secret"},
		{name: "spoiler with reason", html: `<span data-mx-spoiler="plot">secret</span>`, want: "(spoiler: plot) secret"},
		{name: "inline code", html: "<code>x</code>", want: "`x`"},
		{name: "code block", html: `<pre><code class="language-go">a := 1</code></pre>`, want: "```go\na := 1\n```"},
		{name: "link", html: `<a href="https://example.com">site</a>`, want: "[site](https://example.com)"},
		{name: "bare link", html: `<a href="https://example.com">https://example.com</a>`, want: "https://example.com"},
		{name: "unsupported link", html: `<a href="ftp://x">files</a>`, want: "files (ftp://x)"},
		{name: "ghost pill", html: `hi <a href="https://matrix.to/#/@chatto_ualice:example.com">Alice</a>`, want: "hi @alice"},
		{name: "puppet pill", html: `<a href="https://matrix.to/#/@bob:matrix.org">Bob</a> ok`, want: "@bob ok"},
		{name: "unknown pill", html: `<a href="https://matrix.to/#/@x:y.org">Xavier</a>`, want: "Xavier"},
		{name: "invalid handle pill", html: `<a href="https://matrix.to/#/@weird:example.com">Weird</a>`, want: "Weird"},
		{name: "known event link", html: `<a href="https://matrix.to/#/!room:x/$known">link</a>`, want: "https://chat.example.com/chat/-/R1/m/E1"},
		{name: "unknown event link", html: `<a href="https://matrix.to/#/!room:x/$other">link</a>`, want: "https://matrix.to/#/!room:x/$other"},
		{name: "quote", html: "<blockquote>q</blockquote>", want: "> q"},
		{name: "list", html: "<ul><li>a</li><li>b</li></ul>", want: "* a\n* b"},
		{name: "heading", html: "<h2>T</h2>", want: "## T"},
		{name: "emoticon image", html: `<img data-mx-emoticon src="mxc://x/y" alt=":blob:">`, want: ":blob:"},
		{name: "line break", html: "a<br>b", want: "a\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := &event.MessageEventContent{MsgType: event.MsgText, Body: tt.body}
			if tt.html != "" {
				content.Format = event.FormatHTML
				content.FormattedBody = tt.html
			}
			got := FromMatrix(context.Background(), content, testFromMatrixOpts())
			if got != tt.want {
				t.Fatalf("FromMatrix() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRoundTrip checks that common formatting survives Chatto → Matrix → Chatto.
func TestRoundTrip(t *testing.T) {
	inputs := []string{
		"**bold** and *italic*",
		"`code`",
		"~~strike~~",
		"[site](https://example.com)",
		"hi @alice",
		"> quote",
		"# Heading",
		"```go\na := 1\n```",
	}
	for _, in := range inputs {
		mx := ToMatrix(context.Background(), in, testToMatrixOpts())
		back := FromMatrix(context.Background(), mx, testFromMatrixOpts())
		if back != in {
			t.Errorf("round trip %q -> %q -> %q", in, mx.FormattedBody, back)
		}
	}
}
