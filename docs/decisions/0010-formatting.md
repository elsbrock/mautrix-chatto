# 0010: Formatting conversion

Status: accepted (2026-10-25)

## Decision
- **Chatto → Matrix:**
  - goldmark is configured to match Chatto's markdown-it setup: no setext
    headings, horizontal rules, reference links, images or raw HTML.
  - Backslash escapes are disabled the same way Chatto's server does it (by
    doubling backslashes).
  - Single newlines become `<br>`, as with markdown-it's `breaks: true`.
  - Only http, https, mailto and root-relative links are kept; root-relative
    links are made absolute.
  - Mentions and linkification use code ported from Chatto's server, checked
    against its 86 shared test cases.
  - `@all` and `@here` set `m.mentions.room`.
- **Matrix → Chatto:**
  - Uses mautrix's HTML parser with Markdown output.
  - Pills become `@login` for ghosts and for Matrix users with a Chatto login;
    anything else becomes the display name.
  - Links to bridged events become Chatto message links.
  - Chatto has no underline or spoilers, so they are reduced to plain text
    (spoilers keep their reason as a prefix).
  - Emotes become `*text*`.
  - Literal Markdown characters in Matrix plain text cannot be escaped,
    because Chatto disables escapes, so they may render as formatting.
