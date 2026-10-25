package connector

import (
	"strings"

	"golang.org/x/mod/semver"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
)

// CheckServerVersion reports whether a Chatto server version is at least the
// vendored protocol version. Versions that are not semantic versions (e.g.
// development builds) are assumed to be compatible.
func CheckServerVersion(version string) bool {
	v := normalizeVersion(version)
	if !semver.IsValid(v) {
		return true
	}
	return semver.Compare(v, normalizeVersion(chattoclient.UpstreamVersion)) >= 0
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}
