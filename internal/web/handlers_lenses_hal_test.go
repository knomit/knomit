package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// newTestLensManager builds a started repos.Manager (so Registry() is live)
// and provisions the named preset repos. It returns the manager and its Home
// dir so replica tests can copy a repo's .db to force an ID collision.
func newTestLensManager(t *testing.T, names ...string) (*repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, name := range names {
		if _, err := m.Create(context.Background(), repos.CreateSpec{
			Name: name, Mode: "preset", OntologyPreset: "default",
		}, nil); err != nil {
			t.Fatalf("create repo %q: %v", name, err)
		}
	}
	return m, home
}

// cloneRepo provisions a replica of src under dst by copying its .db, so both
// resolve to the same root-commit ID — the shape ValidateLens must reject.
//
// The clone gets its own registry row, and therefore its own uid: lens
// membership is keyed by uid, so a clone with no row could not be named in a
// lens at all and the replica guard would never be reached.
func cloneRepo(t *testing.T, m *repos.Manager, home, src, dst string) {
	t.Helper()
	srcRI := m.Get(src)
	if srcRI == nil {
		t.Fatalf("source repo %q not found", src)
	}
	srcRI.WithRead(func(svc *store.Service) {
		if svc == nil {
			t.Fatalf("source repo %q has no store", src)
		}
		if err := svc.Checkpoint(); err != nil {
			t.Fatalf("checkpoint %q: %v", src, err)
		}
	})
	data, err := os.ReadFile(m.RepoPath(srcRI.UID()))
	if err != nil {
		t.Fatalf("read src db: %v", err)
	}
	uid := "uid-" + dst
	if err := m.Repos().Insert(repos.RepoRecord{
		UID: uid, Name: dst, State: repos.StateActive, Profile: "code", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("register clone %q: %v", dst, err)
	}
	// Same path m.RepoPath(uid) yields — spelled out from home so the fixture
	// shows where a repo's .db actually lives: <home>/repos/<uid>.db.
	dstPath := filepath.Join(home, "repos", uid+".db")
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		t.Fatalf("write dst db: %v", err)
	}
	if err := m.Add(dst, uid, dstPath, nil); err != nil {
		t.Fatalf("add clone %q: %v", dst, err)
	}
}

// lensViewBody mirrors the wire shape of a single lens for decoding in tests.
type lensMemberBody struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type lensViewBody struct {
	Name  string         `json:"name"`
	Write lensMemberBody `json:"write"`
	Reads []struct {
		lensMemberBody
		Branch string `json:"branch"`
		Source string `json:"source"`
	} `json:"reads"`
	Description string      `json:"description"`
	CreatedAt   int64       `json:"created_at"`
	UpdatedAt   int64       `json:"updated_at"`
	Links       hal.LinkMap `json:"_links"`
}

// lensUID is the registry uid of a provisioned test repo — the only spelling the
// lens API accepts for a member. An unregistered name is returned unchanged, so
// a fixture can still send a value that resolves to nothing.
func lensUID(m *repos.Manager, name string) string {
	reg := m.Repos()
	if reg == nil {
		return name // manager never started; the 503 tests run against this
	}
	rec, ok, err := reg.ByName(name)
	if err != nil || !ok {
		return name
	}
	return rec.UID
}

// lensReq rewrites a NAME-spelled lens request body into the uid-spelled form
// the API accepts: `"write":"alpha"` → `"write":{"uid":"<alpha's uid>"}` and
// `{"repo":"beta"}` → `{"uid":"<beta's uid>"}`.
//
// Fixtures know repo names — a test that hard-coded a generated ksuid would be
// unreadable, and one that resolved every uid inline would bury the case it is
// making. The translation lives HERE and not in a handler on purpose: the wire
// has exactly one spelling, and this is a test fixture speaking it.
func lensReq(t *testing.T, m *repos.Manager, body string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("lensReq: %v; body=%s", err, body)
	}
	if w, ok := doc["write"].(string); ok {
		doc["write"] = map[string]any{"uid": lensUID(m, w)}
	}
	if reads, ok := doc["reads"].([]any); ok {
		for _, r := range reads {
			rd, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if repo, ok := rd["repo"].(string); ok {
				delete(rd, "repo")
				rd["uid"] = lensUID(m, repo)
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("lensReq marshal: %v", err)
	}
	return string(out)
}

// postLens POSTs a name-spelled body, translated to the uid wire form.
func postLens(t *testing.T, m *repos.Manager, r http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postLensRaw(t, r, lensReq(t, m, body))
}

// postLensRaw POSTs the body verbatim — for the cases whose whole point is what
// the server does with a body no fixture would translate.
func postLensRaw(t *testing.T, r http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/lenses", bytes.NewBufferString(body))
	r.ServeHTTP(rec, req)
	return rec
}

// Lens JSON carries the uid (the durable key a client sends back) plus a
// resolved display name, so the UI never has to put a uid in front of a human
// and never needs a second fetch to render one. The write repo, which
// normalize() folds into the reads, carries the same pair there.
func TestGetLens_CarriesUIDAndName(t *testing.T) {
	m, _ := newTestLensManager(t, "core")
	r := (&Server{Manager: m}).NewAPIRouter()
	uid := m.Get("core").UID()

	if rec := postLens(t, m, r, `{"name":"eng","write":{"uid":"`+uid+`"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Write struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"write"`
		Reads []struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"reads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Write.UID != uid || body.Write.Name != "core" {
		t.Errorf("write: got {uid:%q name:%q}, want {uid:%q name:%q}", body.Write.UID, body.Write.Name, uid, "core")
	}
	if len(body.Reads) != 1 {
		t.Fatalf("reads: got %d, want 1 (write repo folded in); body=%s", len(body.Reads), rec.Body.String())
	}
	if body.Reads[0].UID != uid || body.Reads[0].Name != "core" {
		t.Errorf("read mount: got {uid:%q name:%q}, want {uid:%q name:%q}",
			body.Reads[0].UID, body.Reads[0].Name, uid, "core")
	}
}

func TestHandleHALLensesCreate_Created(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "eng" || body.Write.Name != "alpha" {
		t.Errorf("name/write: got %q/%q", body.Name, body.Write.Name)
	}
	// normalize() folds the write repo into the reads, so both members appear.
	if len(body.Reads) != 2 {
		t.Fatalf("reads: got %d, want 2 (write repo folded in); body=%s", len(body.Reads), rec.Body.String())
	}
	if body.Links["self"].Href != APIBase+"/lenses/eng" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}

	// Proof CreateLens persisted it (so validation actually ran). The wire and
	// the store now agree: both key membership by alpha's registry uid.
	got, ok, err := m.LensRegistry().Get("eng")
	if err != nil || !ok {
		t.Fatalf("registry Get: ok=%v err=%v", ok, err)
	}
	wantUID := m.Get("alpha").UID()
	if got.WriteUID != wantUID {
		t.Errorf("persisted write uid: got %q, want %q", got.WriteUID, wantUID)
	}
	if got.WriteUID == "alpha" {
		t.Errorf("membership must be stored by uid, not name")
	}
}

// A description POSTed on create is persisted and returned by both the single
// GET and the list GET.
func TestHandleHALLensesCreate_DescriptionRoundTrips(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}],"description":"team knowledge base"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.Description != "team knowledge base" {
		t.Errorf("create description: got %q", created.Description)
	}

	// Single GET.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
	var single lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("unmarshal single: %v", err)
	}
	if single.Description != "team knowledge base" {
		t.Errorf("single-GET description: got %q", single.Description)
	}

	// List GET.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
	var list struct {
		Embedded struct {
			Lenses []lensViewBody `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Embedded.Lenses) != 1 || list.Embedded.Lenses[0].Description != "team knowledge base" {
		t.Errorf("list description: got %+v", list.Embedded.Lenses)
	}
}

// A description over the 4 KiB cap is a well-formed request the server refuses
// → 422 Unprocessable Entity.
func TestHandleHALLensesCreate_DescriptionTooLong(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	desc := strings.Repeat("x", 4097)
	body, err := json.Marshal(map[string]any{
		"name": "eng", "write": "alpha",
		"reads":       []map[string]string{{"repo": "beta"}},
		"description": desc,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postLens(t, m, r, string(body))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}

	// A description exactly at the cap is accepted.
	body, _ = json.Marshal(map[string]any{
		"name": "eng2", "write": "alpha",
		"reads":       []map[string]string{{"repo": "beta"}},
		"description": strings.Repeat("x", 4096),
	})
	if rec := postLens(t, m, r, string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("at-cap status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_RepoNameCollision(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"beta","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

// A member uid that no registered repo has is a 400, not the 422 it used to be:
// with the wire keyed by uid, "no repo has this uid" is a malformed identifier,
// and the caller needs to be told where the right one comes from. The 422 arm
// (repos.ErrRepoNotFound) survives for the race this check cannot close — a
// member archived between here and the manager's validation.
func TestHandleHALLensesCreate_UnknownReadRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"ghost"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if d := problemDetail(t, rec); !strings.Contains(d, "ghost") || !strings.Contains(d, "/repos") {
		t.Errorf("detail must name the bad uid and where uids come from: got %q", d)
	}
}

// An ARCHIVED repo's uid IS a registered uid, so a gate that only asks "does a
// row exist" waves it through — and the caller gets the manager's
// `422 repo not found: "<ksuid>"`, a raw ksuid it was never shown. Replacing
// that answer is the entire reason this gate exists, so it must ask for ACTIVE,
// and it must say the repo is archived rather than that its uid is unknown.
func TestHandleHALLensesCreate_ArchivedReadRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	alphaUID, betaUID := lensUID(m, "alpha"), lensUID(m, "beta")
	if _, err := m.Archive("beta"); err != nil {
		t.Fatalf("archive beta: %v", err)
	}

	rec := postLensRaw(t, r,
		`{"name":"eng","write":{"uid":"`+alphaUID+`"},"reads":[{"uid":"`+betaUID+`"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := problemTitle(t, rec); got != "Archived repo" {
		t.Errorf("title: got %q, want %q", got, "Archived repo")
	}
	if d := problemDetail(t, rec); !strings.Contains(d, "beta") {
		t.Errorf("detail must name the repo the caller archived, not just its ksuid: got %q", d)
	}
	if _, ok, _ := m.LensRegistry().Get("eng"); ok {
		t.Error("a refused create must not have persisted a lens")
	}
}

// A request that spells a member by NAME — the pre-uid contract — is refused
// outright, with a message a client can act on. Accepting it would give the wire
// two spellings whose meanings diverge the moment a repo is renamed.
func TestHandleHALLensesCreate_MemberByNameRejected(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	// write as a bare name string.
	rec := postLensRaw(t, r, `{"name":"eng","write":"alpha","reads":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("write-by-name status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if d := problemDetail(t, rec); !strings.Contains(d, "uid") {
		t.Errorf("write-by-name detail must point at uid: got %q", d)
	}

	// A read mount in the old {"repo": name} shape carries no uid at all;
	// normalize() would drop it silently, so it must be refused instead.
	rec = postLensRaw(t, r, `{"name":"eng","write":{"uid":"`+lensUID(m, "alpha")+`"},"reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("read-by-name status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if d := problemDetail(t, rec); !strings.Contains(d, "uid") {
		t.Errorf("read-by-name detail must point at uid: got %q", d)
	}
	// A mount carrying NO uid is a different mistake from a uid that names
	// nothing, and the title has to say which — a caller reading only titles
	// would otherwise be told to check a uid it never sent.
	if got := problemTitle(t, rec); got != "Missing repo uid" {
		t.Errorf("read-with-no-uid title: got %q, want %q", got, "Missing repo uid")
	}
	rec = postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"ghost"}]}`)
	if got := problemTitle(t, rec); got != "Unknown repo uid" {
		t.Errorf("unresolvable-uid title: got %q, want %q", got, "Unknown repo uid")
	}
	if _, ok, _ := m.LensRegistry().Get("eng"); ok {
		t.Error("a refused create must not have persisted a lens")
	}
}

// Reads come back ordered by resolved NAME, not by the ksuid they are stored
// under — the same order NewBindingOfLens gives the federation, and the only one
// that reads sensibly in the UI's mount list.
func TestGetLens_ReadsSortedByName(t *testing.T) {
	// Provisioned in an order that does NOT match the alphabet, so a
	// creation-ordered (uid-ordered) response would come back zulu, alpha, mike.
	m, _ := newTestLensManager(t, "zulu", "alpha", "mike")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"mike"},{"repo":"alpha"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	got := make([]string, len(body.Reads))
	for i, rd := range body.Reads {
		got[i] = rd.Name
	}
	want := []string{"alpha", "mike", "zulu"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("read order: got %v, want %v", got, want)
	}
}

func TestHandleHALLensesCreate_Replica(t *testing.T) {
	m, home := newTestLensManager(t, "alpha")
	cloneRepo(t, m, home, "alpha", "alpha_clone")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"alpha_clone"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_InvalidName(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, m, r, `{"name":"Bad Name","write":"alpha"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_EmptyWrite(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	// Empty write repo → ErrLensWriteEmpty → 400 (A1). Previously this leaked
	// through member resolution as ErrRepoNotFound → 422.
	rec := postLens(t, m, r, `{"name":"eng","write":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensesCreate_DuplicateName(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}
	// Re-creating the same lens name → ErrLensExists → 409 (backlog C.11).
	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_BadBranchPin(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	// A branch pin the member repo does not have → ErrLensBranchUnknown → 422
	// (backlog C.11).
	rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta","branch":"nope"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_BadJSON(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLensRaw(t, r, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLenses_ListShowsCreated(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q", got)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Lenses []lensViewBody `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 || len(body.Embedded.Lenses) != 1 {
		t.Fatalf("count/embedded: got %d/%d, want 1/1; body=%s", body.Count, len(body.Embedded.Lenses), rec.Body.String())
	}
	if body.Embedded.Lenses[0].Name != "eng" {
		t.Errorf("lens name: got %q", body.Embedded.Lenses[0].Name)
	}
	if body.Links["self"].Href != APIBase+"/lenses" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}
}

func TestHandleHALLenses_EmptyCollection(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Lenses []any `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count: got %d, want 0", body.Count)
	}
	if body.Embedded.Lenses == nil {
		t.Error("embedded lenses should be [] not null")
	}
}

func TestHandleHALLens_GetAndNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses/eng", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "eng" {
		t.Errorf("name: got %q", body.Name)
	}
	if body.Links["self"].Href != APIBase+"/lenses/eng" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}

	// Unknown → 404 problem.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/lenses/missing", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensDelete_DeleteAndNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/lenses/eng", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := m.LensRegistry().Get("eng"); ok {
		t.Error("lens should be gone after delete")
	}

	// Deleting an unknown lens → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/lenses/missing", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: got %d, want 404", rec.Code)
	}
}

// problemTitle decodes the "title" field of a problem+json body.
func problemTitle(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v; body=%s", err, rec.Body.String())
	}
	return p.Title
}

// problemDetail decodes the "detail" field of a problem+json body.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v; body=%s", err, rec.Body.String())
	}
	return p.Detail
}

// TestLensCreateErrStatus pins the sentinel→(status,title) mapping. The
// ErrCreateInFlight arm is the FIX-1 case: a concurrent same-name create (or a
// lens create racing a repo create for the same name) must map to 409, not the
// 500 default. This mapper is the only deterministic way to exercise that arm —
// the in-flight reservation seam (reserveNameAndOrigin) is unexported.
func TestLensCreateErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", repos.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write empty", repos.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", repos.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", repos.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Create lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensCreateErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}

// TestLensPatchErrStatus pins the PATCH mapping (m13): the validation arms are
// byte-identical to lensCreateErrStatus, but the scrubbed-500 default arm carries
// an operation-appropriate title so a PATCH failure never reads "Create lens
// failed".
func TestLensPatchErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		// 4xx/422 arms mirror create exactly (byte-identity is a hard constraint).
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", repos.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write empty", repos.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", repos.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", repos.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		// The only divergence: the unmapped 500 default arm names the PATCH op.
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Update lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensPatchErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}

// closeControlDB shuts the single control.db handle Manager.Start opens, while
// leaving LensRegistry() non-nil. It goes through Repos() rather than
// LensRegistry().Close() because the lens registry now BORROWS the repo
// registry's handle — its own Close is a deliberate no-op, so it cannot break
// anything. sql.DB.Close is idempotent, so the manager's own Close still works.
func closeControlDB(t *testing.T, m *repos.Manager) {
	t.Helper()
	if err := m.Repos().DB().Close(); err != nil {
		t.Fatalf("close control db: %v", err)
	}
}

// TestHandleHALLenses_500DoesNotLeakError forces a real registry-layer failure
// (closing the control-plane DB while Registry() stays non-nil) and asserts the
// 500 problem detail is a generic string, never the wrapped SQL/driver error
// (FIX 2). Each lens handler's 500 fall-through is covered.
func TestHandleHALLenses_500DoesNotLeakError(t *testing.T) {
	leak := "database is closed" // substring of the raw sql driver error

	t.Run("list", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		closeControlDB(t, m)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "list lenses failed" || strings.Contains(d, leak) {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})

	t.Run("get", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		closeControlDB(t, m)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "get lens failed" || strings.Contains(d, leak) {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})

	t.Run("delete", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		closeControlDB(t, m)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/lenses/eng", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		// The delete handler's Get-check fails first, so the detail is the
		// get-generic; either way it must not leak the driver error.
		if d := problemDetail(t, rec); strings.Contains(d, leak) {
			t.Errorf("detail leaked: %q", d)
		}
	})

	t.Run("create", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha", "beta")
		r := (&Server{Manager: m}).NewAPIRouter()
		// Drop the lens table rather than closing the handle: every tenant now
		// shares one control.db connection, so closing it would fail MEMBER
		// RESOLUTION first and the create handler's own 500 scrub would never be
		// reached. With `repos` intact, resolution succeeds and CreateLens is
		// what fails — which is the arm under test.
		if _, err := m.Repos().DB().Exec(`DROP TABLE lenses`); err != nil {
			t.Fatalf("drop lenses: %v", err)
		}
		rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "create lens failed" || strings.Contains(d, "no such table") {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})
}

func TestHandleHALLenses_RegistryNil503(t *testing.T) {
	// A manager that was never Started has a nil registry.
	m := repos.New(context.Background(), repos.Deps{})
	r := (&Server{Manager: m}).NewAPIRouter()

	// list
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list: got %d, want 503", rec.Code)
	}
	// get
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("get: got %d, want 503", rec.Code)
	}
	// create
	rec = postLens(t, m, r, `{"name":"eng","write":"alpha"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create: got %d, want 503", rec.Code)
	}
	// delete
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/lenses/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("delete: got %d, want 503", rec.Code)
	}
}
