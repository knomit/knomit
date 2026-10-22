package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

const activityURL = "/repos/alpha/branches/agent:test/activity"

type activityWire struct {
	LastCommit string      `json:"last_commit"`
	Total      int         `json:"total"`
	Changes7d  int         `json:"changes_7d"`
	Changes30d int         `json:"changes_30d"`
	Changes90d int         `json:"changes_90d"`
	Links      hal.LinkMap `json:"_links"`
}

func decodeActivity(t *testing.T, body []byte) activityWire {
	t.Helper()
	var out activityWire
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out
}

// Three facts committed just now: the repo activity gains exactly three commits, in every
// window, and names the time of the last commit as a UTC RFC 3339 stamp.
func TestHandleHALActivity_ReturnsHAL(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	// InitRepo commits once itself; count against that baseline.
	base := decodeActivity(t, getAPI(t, &Server{Manager: m}, activityURL).Body.Bytes())
	for _, p := range []string{"kb/a/one.md", "kb/a/two.md", "kb/a/three.md"} {
		writeTypedFact(t, a, p, p, fact.Observation, 0.9, nil, nil, nil)
	}

	rec := getAPI(t, &Server{Manager: m}, activityURL)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	body := decodeActivity(t, rec.Body.Bytes())
	require.Equal(t, base.Total+3, body.Total)
	require.Equal(t, base.Changes7d+3, body.Changes7d)
	require.Equal(t, base.Changes30d+3, body.Changes30d)
	require.Equal(t, base.Changes90d+3, body.Changes90d)
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, body.LastCommit)
	require.Equal(t, APIBase+activityURL, body.Links["self"].Href)
}

// path= both narrows the activity to the subtree and is echoed in the self
// link.
func TestHandleHALActivity_WithPath_NarrowsAndIncludesQueryInSelf(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	a := m.Get("alpha")
	writeTypedFact(t, a, "kb/know/ai/one.md", "One", fact.Observation, 0.9, nil, nil, nil)
	writeTypedFact(t, a, "kb/know/ai/two.md", "Two", fact.Observation, 0.9, nil, nil, nil)
	writeTypedFact(t, a, "kb/other/three.md", "Three", fact.Observation, 0.9, nil, nil, nil)
	s := &Server{Manager: m}

	whole := decodeActivity(t, getAPI(t, s, activityURL).Body.Bytes())
	require.Greater(t, whole.Total, 3, "the whole-repo activity also counts the commit outside the subtree")

	rec := getAPI(t, s, activityURL+"?path=kb/know/ai")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeActivity(t, rec.Body.Bytes())
	require.Equal(t, 2, body.Total)
	require.Equal(t, APIBase+activityURL+"?path=kb/know/ai", body.Links["self"].Href)
}

func TestHandleHALActivity_UnknownRepo_Returns404(t *testing.T) {
	m, _ := newRealStoreManager(t)

	rec := getAPI(t, &Server{Manager: m}, "/repos/missing/branches/main/activity")

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

// The store's Activity read fails: the handler answers 500 carrying THAT error,
// and the fault fired for the Activity read and no other.
func TestHandleHALActivity_StoreError_Returns500(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injected := errors.New("injected activity read failure")
	var hits atomic.Int64
	t.Cleanup(svcs["alpha"].Search().SetReadFaultForTest(failIndexRead(index.ReadActivity, injected, &hits)))

	rec := getAPI(t, &Server{Manager: m}, activityURL)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "the Activity read must have reached the hook exactly once")
}
