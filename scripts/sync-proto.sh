#!/usr/bin/env bash
# Re-vendors Chatto's public protos at the version in
# pkg/chattoclient/client.go (UpstreamVersion) and regenerates the Go code.
#
# Usage: scripts/sync-proto.sh [new-version]
# With a version argument, UpstreamVersion is bumped first.
set -euo pipefail
cd "$(dirname "$0")/.."

client=pkg/chattoclient/client.go
if [[ $# -ge 1 ]]; then
	sed -i -E "s/^const UpstreamVersion = \".*\"/const UpstreamVersion = \"$1\"/" "$client"
fi
version=$(sed -nE 's/^const UpstreamVersion = "(.*)"/\1/p' "$client")
echo "Syncing protos from chattocorp/chatto $version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git clone --quiet --depth 1 --branch "$version" https://github.com/chattocorp/chatto "$tmp/chatto"

for pkg in api admin discovery realtime operator; do
	rm -rf "proto/chatto/$pkg"
	cp -r "$tmp/chatto/proto/chatto/$pkg" proto/chatto/
done
find proto -name AGENTS.md -delete
cp "$tmp/chatto/proto/buf.lock" proto/buf.lock

# Keep the mention test cases in sync with the server's.
cp "$tmp/chatto/testdata/mentions/extraction.json" pkg/chattofmt/testdata/mentions_extraction.json

rm -rf pkg/chattopb
buf generate
echo "Done. Review the diff, run the tests, and update docs/chatto-protocol-notes.md if behaviour changed."
