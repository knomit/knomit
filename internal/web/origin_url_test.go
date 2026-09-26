package web

import (
	"encoding/json"
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
				r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, endpoint,
					strings.NewReader(`{"url":"`+url+`"}`))))
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

// whitespaceForms are the five paddings, JSON-escaped because they go into a
// request body verbatim. Fed separately everywhere, never as one mixed string:
// the forms behave DIFFERENTLY without the trim, so a mixed string can pass on
// the strength of whichever one happens to work.
var whitespaceForms = map[string]func(string) string{
	"trailing space": func(u string) string { return u + " " },
	"leading space":  func(u string) string { return " " + u },
	"tab":            func(u string) string { return `\t` + u + `\t` },
	"newline":        func(u string) string { return u + `\n` },
	"all of them":    func(u string) string { return ` \t\n` + u + ` \t\n` },
}

// THE WIRING, not the helper. The three entry points below carry the trim but
// had no assertion: the reviewer removed the call from all three at once and
// internal/web stayed green. Neutering trimOriginURL fails loudly and proves
// nothing about its callers, and "one helper applied at every entry point"
// makes the CALLERS the invariant — they are what a future refactor of any one
// handler would drop.
//
// Each row observes the URL that actually left the handler: the paths a
// recording remote was asked for, the row that landed in control.db, or the
// URL the session stored.
func TestOriginURL_EveryEntryPointTrims(t *testing.T) {
	// POST /repos. CreatePreflight's last check touches the network
	// (ProbeOriginRefs), synchronously, before the 202 — so a recording remote
	// sees the URL the Manager was given.
	//
	// Each form gets its OWN remote and its own repo name: preflight refuses a
	// second create against an origin already in use, so reusing one remote
	// would make every row after the first fail for the wrong reason.
	t.Run("POST /repos (create preflight)", func(t *testing.T) {
		probePaths := func(t *testing.T, name string, pad func(string) string) []string {
			t.Helper()
			clean, paths, _ := recordingRemote(t)
			s := &Server{Manager: newRealManager(t)}
			rec := httptest.NewRecorder()
			s.NewAPIRouter().ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos",
				strings.NewReader(`{"name":"`+name+`","mode":"clone","origin":{"url":"`+pad(clean)+`"}}`))))
			return paths()
		}

		want := probePaths(t, "cleanrepo", func(u string) string { return u })
		require.NotEmpty(t, want, "the clean URL must actually have reached the remote")

		for name, pad := range whitespaceForms {
			t.Run(name, func(t *testing.T) {
				require.Equal(t, want, probePaths(t, "padrepo", pad),
					"a URL padded with %s must be probed at the same paths as the clean one", name)
			})
		}
	})

	// PUT /repos/{repo}/origin PERSISTS the URL without a network round trip
	// first — the handler's own comment says the origin row is kept even when
	// the following ActivateSync fails. An untrimmed URL here is STORED, and
	// becomes the identity key ActiveRepoWithOrigin matches on, so this is the
	// entry point whose trim most needs pinning.
	//
	// Adapted from the reviewer's own fixture, which is better than what this
	// table had first: the assertion is on what lands in control.db, NOT on
	// the response status. This endpoint answers 502 either way — the
	// example.invalid host never resolves, so ActivateSync fails regardless —
	// so a status assertion would pin nothing at all.
	t.Run("PUT /repos/{repo}/origin", func(t *testing.T) {
		const clean = "https://example.invalid/kb.git"
		for name, pad := range whitespaceForms {
			t.Run(name, func(t *testing.T) {
				s, m, ri := newControlDBTestServer(t, t.TempDir())
				rec := httptest.NewRecorder()
				req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/origin",
					strings.NewReader(`{"url":"`+pad(clean)+`","branch":"main","auth_method":"token","token":"tok"}`)))
				req.Header.Set("Content-Type", "application/json")
				s.NewAPIRouter().ServeHTTP(rec, req)

				origin, err := m.Origins().Get(ri.UID())
				require.NoError(t, err)

				// Without the trim these forms fail in TWO different ways, and
				// the message has to say which or a reader debugs the wrong
				// thing. A bare "expected not nil" across four rows reads like
				// a broken fixture rather than a result:
				//   trailing space       isGitURL ACCEPTS it (url.Parse does),
				//                        so the padded URL is PERSISTED — the
				//                        originally reported bug.
				//   leading space/tab/\n isGitURL REJECTS them, so the handler
				//                        400s and NOTHING is persisted.
				require.NotNil(t, origin,
					"a URL padded with %s was REJECTED before persisting (status %d, %s) — "+
						"the trim must make it acceptable, not merely clean",
					name, rec.Code, strings.TrimSpace(rec.Body.String()))
				require.Equal(t, clean, origin.URL,
					"a URL padded with %s was PERSISTED with its padding intact", name)
			})
		}
	})

	// POST /repos/{repo}/origin-sessions opens without touching the network —
	// the fetch is the later /test step — so the observable is the URL the
	// session STORED, which every later step of the flow uses. The list
	// endpoint reports it. isGitURL guards this endpoint too, so the same
	// two-way split applies without the trim.
	t.Run("POST /repos/{repo}/origin-sessions", func(t *testing.T) {
		const clean = "https://example.invalid/kb.git"
		for name, pad := range whitespaceForms {
			t.Run(name, func(t *testing.T) {
				s := &Server{
					Manager:        newTestManagerWithRepos(t, "alpha"),
					SessionManager: NewSessionManager(),
					providers:      storeProviders{origin: &stubOriginProvider{}},
				}
				t.Cleanup(s.SessionManager.Shutdown)
				r := s.NewAPIRouter()

				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions",
					strings.NewReader(`{"url":"`+pad(clean)+`","auth_method":"none"}`))))
				require.Equal(t, http.StatusOK, rec.Code,
					"a URL padded with %s was REJECTED before a session opened (%s) — "+
						"the trim must make it acceptable, not merely clean",
					name, strings.TrimSpace(rec.Body.String()))

				list := httptest.NewRecorder()
				r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions", nil))
				require.Equal(t, http.StatusOK, list.Code, list.Body.String())

				var got struct {
					Embedded struct {
						Sessions []struct {
							URL string `json:"url"`
						} `json:"sessions"`
					} `json:"_embedded"`
				}
				require.NoError(t, json.Unmarshal(list.Body.Bytes(), &got))
				require.Len(t, got.Embedded.Sessions, 1)
				require.Equal(t, clean, got.Embedded.Sessions[0].URL,
					"a session opened with a URL padded with %s STORED the padding", name)
			})
		}
	})
}

// A url of nothing but whitespace is the same as no url at all: "required",
// not a probe of a whitespace address.
func TestOriginURL_WhitespaceOnlyURLIsRejectedAsMissing(t *testing.T) {
	for _, endpoint := range []string{"/repos:probe-origin", "/repos:probe-initialized"} {
		t.Run(endpoint, func(t *testing.T) {
			s := &Server{Manager: newRealManager(t)}
			r := s.NewAPIRouter()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, endpoint,
				strings.NewReader(`{"url":"  \t "}`))))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "url is required")
		})
	}
}
