# 0002: Licensing of the vendored protos

Status: accepted (2026-10-25)

## Context
Chatto is AGPL-3.0-or-later by default. Its `REUSE.toml` lists
`proto/chatto/{admin,api,auth,discovery,operator,realtime,search}/**` and the
buf config files under an Apache-2.0 annotation ("public protocol/API
definitions").

## Decision
- The vendored protos keep their Apache-2.0 licence; `proto/LICENSE` holds the text.
- The bridge itself is AGPL-3.0-or-later, like the mautrix bridges it is
  built from (mautrix-go is MPL-2.0; the bridges are AGPL).
- `pkg/chattofmt/mentions.go` ports logic from Chatto's AGPL server code
  (mention scanning and URL detection) with attribution. That is compatible
  because the bridge is AGPL too.
- `pkg/chattofmt/testdata/mentions_extraction.json` is copied from Chatto's
  `testdata/mentions/`, which `REUSE.toml` lists as Apache-2.0.
- `pkg/emoji/emoji_gen.go` is generated from Chatto's emoji catalogue (AGPL),
  which is built from gemoji (MIT) and Unicode's skin-tone sequences (Unicode
  License). The generated file names its sources.

## Consequences
Anyone forking the bridge under a permissive licence must replace
`mentions.go` and generate `pkg/emoji/emoji_gen.go` from gemoji and
Unicode's data directly. The mention test data can stay.
