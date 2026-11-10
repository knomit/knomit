package manage

import (
	"testing"

	"knomit/internal/edge/web/httpx"
)

// The route permission resolver for POST /repos and :probe-initialized peeks
// at most httpx.RepoCreateBodyMaxBytes; the handlers accept up to
// maxCreateBodyBytes. If they drift apart, a legitimate large create would
// resolve to the strongest permission (resolver limit smaller) or the
// resolver would buffer more than the handler ever reads (larger).
func TestRepoCreateBodyLimit_MatchesTheResolver(t *testing.T) {
	if maxCreateBodyBytes != httpx.RepoCreateBodyMaxBytes {
		t.Fatalf("maxCreateBodyBytes = %d, httpx.RepoCreateBodyMaxBytes = %d", maxCreateBodyBytes, httpx.RepoCreateBodyMaxBytes)
	}
}
