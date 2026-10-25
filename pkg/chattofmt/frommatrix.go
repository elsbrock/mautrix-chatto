package chattofmt

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// FromMatrixOptions configures Matrix → Chatto conversion.
type FromMatrixOptions struct {
	// ResolveUser maps a Matrix user ID to a Chatto login so that a pill can
	// become an @mention. It covers ghosts and Matrix users with a login.
	ResolveUser func(ctx context.Context, userID id.UserID) (login string, ok bool)
	// ResolveEventLink maps a Matrix event permalink to a Chatto message URL.
	ResolveEventLink func(ctx context.Context, roomID id.RoomID, eventID id.EventID) (string, bool)
}

// FromMatrix converts Matrix message content into a Chatto Markdown body.
func FromMatrix(ctx context.Context, content *event.MessageEventContent, opts *FromMatrixOptions) string {
	if opts == nil {
		opts = &FromMatrixOptions{}
	}
	if content.Format != event.FormatHTML || content.FormattedBody == "" {
		return content.Body
	}
	parser := &format.HTMLParser{
		TabsToSpaces:   4,
		Newline:        "\n",
		HorizontalLine: "\n---\n",
		BoldConverter: func(s string, _ format.Context) string {
			return wrapInline(s, "**")
		},
		ItalicConverter: func(s string, _ format.Context) string {
			return wrapInline(s, "*")
		},
		StrikethroughConverter: func(s string, _ format.Context) string {
			return wrapInline(s, "~~")
		},
		// Chatto has no underline or spoilers; keep the text.
		UnderlineConverter: func(s string, _ format.Context) string { return s },
		SpoilerConverter: func(text, reason string, _ format.Context) string {
			if reason != "" {
				return fmt.Sprintf("(spoiler: %s) %s", reason, text)
			}
			return text
		},
		LinkConverter: func(text, href string, _ format.Context) string {
			return convertLink(text, href)
		},
		ImageConverter: func(src, alt, title, width, height string, isEmoji bool) string {
			if alt != "" {
				return alt
			}
			return title
		},
		PillConverter: func(displayname, mxid, eventID string, fctx format.Context) string {
			return convertPill(fctx.Ctx, opts, displayname, mxid, eventID, fctx)
		},
	}
	md, _ := format.HTMLToMarkdownFull(parser, content.FormattedBody)
	return strings.TrimSpace(md)
}

// wrapInline adds delimiters around s while keeping surrounding whitespace
// outside, because CommonMark does not allow "** bold**".
func wrapInline(s, delim string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	lead := s[:strings.Index(s, trimmed)]
	trail := s[len(lead)+len(trimmed):]
	return lead + delim + trimmed + delim + trail
}

func convertLink(text, href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return text
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
	default:
		if text == href {
			return text
		}
		return fmt.Sprintf("%s (%s)", text, href)
	}
	plainHref := strings.TrimPrefix(href, "mailto:")
	if text == href || text == plainHref || "https://"+text == href || "http://"+text == href {
		if strings.EqualFold(u.Scheme, "mailto") {
			return fmt.Sprintf("[%s](%s)", text, href)
		}
		return href
	}
	return fmt.Sprintf("[%s](%s)", strings.ReplaceAll(text, "]", ")"), href)
}

func convertPill(ctx context.Context, opts *FromMatrixOptions, displayname, mxid, eventID string, fctx format.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	switch {
	case strings.HasPrefix(mxid, "@"):
		fctx.Mentions().Add(id.UserID(mxid))
		if opts.ResolveUser != nil {
			if login, ok := opts.ResolveUser(ctx, id.UserID(mxid)); ok && IsValidHandle(login) {
				return "@" + login
			}
		}
		return displayname
	case eventID != "":
		if opts.ResolveEventLink != nil && strings.HasPrefix(mxid, "!") {
			if link, ok := opts.ResolveEventLink(ctx, id.RoomID(mxid), id.EventID(eventID)); ok {
				return link
			}
		}
		return fmt.Sprintf("https://matrix.to/#/%s/%s", mxid, eventID)
	case strings.HasPrefix(mxid, "#"):
		return mxid
	default:
		if displayname == mxid {
			return "https://matrix.to/#/" + mxid
		}
		return fmt.Sprintf("[%s](https://matrix.to/#/%s)", displayname, mxid)
	}
}
