package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

func TestTrimOriginURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"leading spaces", "  https://h/r", "https://h/r"},
		{"trailing spaces", "https://h/r  ", "https://h/r"},
		{"both, with a tab", " \thttps://h/r \t", "https://h/r"},
		{"newline from a paste", "https://h/r\n", "https://h/r"},
		{"CRLF from a paste", "https://h/r\r\n", "https://h/r"},
		{"only whitespace is empty", " \t\n ", ""},
		{"clean is untouched", "https://h/r", "https://h/r"},
		// Everything below CHANGES which repository is named, so none of it is
		// this helper's business — the git route tolerates the suffix and the
		// slash on the serving side instead.
		{"inner spaces are not touched", "https://h/a b", "https://h/a b"},
		{"trailing slash is kept", "https://h/r/", "https://h/r/"},
		{"dot-git suffix is kept", "https://h/r.git", "https://h/r.git"},
		{"case is kept", "https://H/R", "https://H/R"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, trimOriginURL(tc.in))
		})
	}
}

// recordingRemote serves a real knomit store and records every path it is
// asked for, so a test can see the URL that actually left the handler rather
// than only whether the call happened to work.
func recordingRemote(t *testing.T) (url string, paths func() []string, reset func()) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{repos.OntologyPath: string(ont)}, "main"))

	var mu sync.Mutex
	var seen []string
	h := svc.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL,
		func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), seen...)
		},
		func() {
			mu.Lock()
			defer mu.Unlock()
			seen = nil
		}
}

// A URL pasted with surrounding whitespace must reach the remote as the same
// URL as the clean one.
//
// This is the reported defect: a URL pasted into the desktop wizard with
// trailing spaces produced
// `GET /git/arxiv-kb%20%20/info/refs?service=git-upload-pack` → 404, reported
// to the user as "repository not found: 404 page not found". The user was
// looking at a correct URL and being told the repository did not exist.
//
// The assertion is on the PATHS THE REMOTE WAS ASKED FOR, not on whether the
// probe succeeded: a test that only checked the outcome would still pass if
// the trim moved somewhere that left a different URL on the wire.
//
// THE WIDENING IS DELIBERATE. Before this change the four whitespace forms
// behaved differently from each other: only a TRAILING space survived
// url.Parse and reached the remote as a mangled path, while a leading space, a
// tab and a newline were rejected outright by isGitURL at the two endpoints
// that validate, and went unvalidated at the other three. After the trim all
// four are accepted everywhere, as the same clean URL. Each form is fed
// separately here rather than as one mixed string, so the table cannot pass on
// the strength of whichever form happens to work.
func TestOriginURL_PaddedURLReachesTheSameRemotePath(t *testing.T) {
	// JSON-escaped, because these go into a request body verbatim.
	pads := map[string]struct{ lead, trail string }{
		"trailing space": {"", " "},
		"leading space":  {" ", ""},
		"tab":            {`\t`, `\t`},
		"newline":        {`\n`, `\n`},
		"all of them":    {` \t\n`, ` \t\n`},
	}
	for _, endpoint := range []string{"/repos:probe-origin", "/repos:probe-initialized"} {
		t.Run(endpoint, func(t *testing.T) {
			clean, paths, reset := recordingRemote(t)
			s := &Server{Manager: newRealManager(t)}
			r := s.NewAPIRouter()

			post := func(url string) int {
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, endpoint,
					strings.NewReader(`{"url":"`+url+`"}`)))
				return rec.Code
			}

			require.Equal(t, http.StatusOK, post(clean))
			want := paths()
			require.NotEmpty(t, want, "the clean URL must actually have reached the remote")

			for name, pad := range pads {
				t.Run(name, func(t *testing.T) {
					reset()
					require.Equal(t, http.StatusOK, post(pad.lead+clean+pad.trail))
					require.Equal(t, want, paths(),
						"a URL padded with %s must be requested at the same paths as the clean one", name)
				})
			}
		})
	}
}

// A url of nothing but whitespace is the same as no url at all: "required",
// not a probe of a whitespace address.
func TestOriginURL_WhitespaceOnlyURLIsRejectedAsMissing(t *testing.T) {
	for _, endpoint := range []string{"/repos:probe-origin", "/repos:probe-initialized"} {
		t.Run(endpoint, func(t *testing.T) {
			s := &Server{Manager: newRealManager(t)}
			r := s.NewAPIRouter()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, endpoint,
				strings.NewReader(`{"url":"  \t "}`)))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "url is required")
		})
	}
}
