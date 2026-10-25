package chattofmt

import (
	"context"
	"strings"

	"github.com/yuin/goldmark/text"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// ToMatrixOptions configures Chatto → Matrix conversion.
type ToMatrixOptions struct {
	// ServerURL is the Chatto base URL, used to absolutise root-relative links.
	ServerURL string
	// ResolveMention maps a written @handle (a login, role name, "here" or
	// "all") to a Matrix user. Unresolved handles stay plain text.
	ResolveMention func(ctx context.Context, handle string) (MentionTarget, bool)
}

// ToMatrix converts a Chatto Markdown body into Matrix message content. The
// result has no msgtype set; callers decide between m.text and m.notice.
func ToMatrix(ctx context.Context, body string, opts *ToMatrixOptions) *event.MessageEventContent {
	if opts == nil {
		opts = &ToMatrixOptions{}
	}
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	if strings.TrimSpace(body) == "" {
		return content
	}
	source := markdownSource(body)
	doc := chattoMarkdown.Parser().Parse(text.NewReader(source))
	mergeAdjacentText(doc)

	mentions := make(map[string]MentionTarget)
	var userIDs []id.UserID
	if opts.ResolveMention != nil {
		for _, handle := range ExtractMentions(body) {
			target, ok := opts.ResolveMention(ctx, handle)
			if !ok || target.UserID == "" {
				continue
			}
			mentions[handle] = target
			userIDs = append(userIDs, id.UserID(target.UserID))
		}
	}

	r := &htmlRenderer{source: source, urls: newURLIndex(source), opts: opts, mentions: mentions}
	r.render(doc)
	content.Mentions = &event.Mentions{}
	if len(userIDs) > 0 {
		content.Mentions.UserIDs = dedupeUserIDs(userIDs)
	}
	htmlBody := unwrapSingleParagraph(r.buf.String())
	plain := format.HTMLToText(htmlBody)
	if !r.formatted && plain == body {
		return content
	}
	content.Format = event.FormatHTML
	content.FormattedBody = htmlBody
	content.Body = plain
	return content
}

func unwrapSingleParagraph(s string) string {
	if strings.HasPrefix(s, "<p>") && strings.HasSuffix(s, "</p>") && strings.Count(s, "<p>") == 1 {
		return s[3 : len(s)-4]
	}
	return s
}

func dedupeUserIDs(ids []id.UserID) []id.UserID {
	seen := make(map[id.UserID]struct{}, len(ids))
	out := ids[:0]
	for _, u := range ids {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}
