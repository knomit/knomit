package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// lensMember is how a lens names one of its member repos on the wire: the uid
// is the durable key clients send back, name is DERIVED and read-only. Both are
// present so a client can render a lens without a second round trip and without
// a lookup race against a concurrent rename.
//
// Requests carry the SAME object so a GET response can be edited and PATCHed
// back verbatim; name is ignored on input.
type lensMember struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// UnmarshalJSON accepts the member object and rejects the pre-uid spelling — a
// bare repo name string — by hand. Left to the stock decoder that request fails
// with "cannot unmarshal string into Go struct field ... of type
// web.lensMember", which tells a caller nothing about what to send instead.
func (lm *lensMember) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		return fmt.Errorf("lens members are identified by uid, not by name (got %q): "+
			"send {\"uid\": …} using the uid from GET %s/repos", name, APIBase)
	}
	// Alias sheds the method set, so this decode does not recurse.
	type plain lensMember
	return json.Unmarshal(data, (*plain)(lm))
}

// lensReadDTO is the wire shape for one read mount of a lens: the member pair
// plus the mount's own settings. The domain model (repos.LensRead) stays
// wire-agnostic; this DTO carries the json tags.
//
// uid/name are spelled out rather than embedding lensMember: embedding promotes
// lensMember's UnmarshalJSON to this struct, and decoding a mount through it
// would silently drop branch and source.
type lensReadDTO struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
	Source string `json:"source,omitempty"`
}

// lensView is the HAL representation of a lens.
type lensView struct {
	Name        string        `json:"name"`
	Write       lensMember    `json:"write"`
	Description string        `json:"description,omitempty"`
	Reads       []lensReadDTO `json:"reads"`
	CreatedAt   int64         `json:"created_at"`
	UpdatedAt   int64         `json:"updated_at"`
	Links       hal.LinkMap   `json:"_links"`
}

// createLensRequest is the POST body for creating a lens.
type createLensRequest struct {
	Name        string        `json:"name"`
	Write       lensMember    `json:"write"`
	Description string        `json:"description"`
	Reads       []lensReadDTO `json:"reads"`
}

// patchLensRequest is the PATCH body for editing a lens. Every field is a
// pointer so an omitted field (JSON key absent → nil) is distinguishable from a
// provided-but-empty one: omitted = keep the current value, provided = replace it
// wholesale (reads replace as a set, never merge). The name is immutable and has
// no field here.
type patchLensRequest struct {
	Write       *lensMember    `json:"write"`
	Description *string        `json:"description"`
	Reads       *[]lensReadDTO `json:"reads"`
}

// lensNames is a uid → display-name index for ONE response. Membership is
// uid-keyed and every response resolves a name per member, so a list of L
// lenses averaging M mounts would otherwise be L×(M+1) registry reads on a
// SetMaxOpenConns(1) handle. One List replaces all of them.
type lensNames map[string]string

// newLensNames indexes every ACTIVE repo. Active is the whole population a lens
// can legally draw on — Archive refuses to touch a lens-referenced repo — so a
// uid missing here is a broken invariant, not a normal state, and member()
// degrades to showing the raw uid rather than failing the response.
//
// A nil registry (Manager not started) yields an empty index for the same
// reason: a view is not the place to discover the control plane is down.
func newLensNames(reg *repos.Registry) (lensNames, error) {
	if reg == nil {
		return lensNames{}, nil
	}
	recs, err := reg.List(repos.StateActive)
	if err != nil {
		return nil, err
	}
	names := make(lensNames, len(recs))
	for _, rec := range recs {
		names[rec.UID] = rec.Name
	}
	return names, nil
}

// member renders a stored member uid as the {uid, name} pair. An unindexed uid
// renders its own uid as the name: the membership row is real, and showing the
// uid is a legible symptom where an empty string would not be.
func (n lensNames) member(uid string) lensMember {
	if name, ok := n[uid]; ok {
		return lensMember{UID: uid, Name: name}
	}
	return lensMember{UID: uid, Name: uid}
}

// lensViewOf renders one lens against an already-built name index.
//
// Reads come back sorted by resolved NAME, not by the uid the rows are stored
// under: uids are ksuids, whose order within a second is arbitrary, and this
// list is what the UI shows a human. It is also exactly the mount order
// NewBindingOfLens produces for the federation tie-break, so the lens editor
// and the lens's own results agree.
func lensViewOf(b hal.URLBuilder, names lensNames, l repos.Lens) lensView {
	reads := make([]lensReadDTO, len(l.Reads))
	for i, r := range l.Reads {
		mem := names.member(r.RepoUID)
		reads[i] = lensReadDTO{UID: mem.UID, Name: mem.Name, Branch: r.Branch, Source: r.Source}
	}
	sort.Slice(reads, func(i, j int) bool {
		if reads[i].Name != reads[j].Name {
			return reads[i].Name < reads[j].Name
		}
		return reads[i].UID < reads[j].UID
	})
	return lensView{
		Name: l.Name, Write: names.member(l.WriteUID), Description: l.Description, Reads: reads,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		Links: hal.LinkMap{"self": {Href: b.Lens(l.Name)}},
	}
}

// lensViewsOf resolves the names for a whole response with one registry read,
// then renders every lens in it.
func lensViewsOf(b hal.URLBuilder, reg *repos.Registry, ls []repos.Lens) ([]lensView, error) {
	names, err := newLensNames(reg)
	if err != nil {
		return nil, err
	}
	views := make([]lensView, 0, len(ls))
	for _, l := range ls {
		views = append(views, lensViewOf(b, names, l))
	}
	return views, nil
}

// writeLensView renders a single lens, or a 500 problem when the name index
// cannot be built.
//
// On the create/patch paths the lens is already PERSISTED by the time this runs,
// so this 500 says "the write succeeded, the response did not" — the caller's
// retry finds the lens there. The alternative, serving uids as names, would look
// like success and be wrong.
func writeLensView(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, reg *repos.Registry, status int, l repos.Lens) {
	views, err := lensViewsOf(b, reg, []repos.Lens{l})
	if err != nil {
		log.Error().Err(err).Str("path", r.URL.Path).Str("lens", l.Name).Msg("resolve lens member names failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Lookup failed", "resolve lens member names failed", r.URL.Path)
		return
	}
	hal.WriteHAL(w, status, views[0])
}

// checkLensMembers verifies that every member a request names is a registered
// repo, identified by uid, returning the problem TITLE and detail for the first
// that is not. Two distinct failures, two distinct titles: a uid that names
// nothing ("Unknown repo uid") is not the same mistake as a mount that carries
// no uid at all ("Missing repo uid"), and a caller reading only the title
// should still be told which one it made.
//
// This is the whole reason the wire has one spelling: a name that happened to
// resolve would make a lens mean something different after a rename, and a name
// that did not resolve used to reach the manager as a bogus uid and come back as
// a 422 about an "unknown repo" — technically true, useless as guidance. Both
// are now a 400 that names the fix.
//
// An empty WRITE is skipped because it has its own sentinel (ErrLensWriteEmpty
// → 400 "Lens write repo required"), which says more than this message does. An
// empty READ uid is NOT skipped: it is what a client sending the old
// {"repo": name} spelling produces, and normalize() would drop that mount
// silently — a lens quietly missing a member is worse than a 400.
//
// A nil registry cannot answer, so the check defers to the manager's validation.
func checkLensMembers(reg *repos.Registry, write string, reads []lensReadDTO) (title, detail string, err error) {
	if reg == nil {
		return "", "", nil
	}
	known := func(uid string) (string, string, error) {
		if _, ok, err := reg.Get(uid); err != nil {
			return "", "", err
		} else if !ok {
			return "Unknown repo uid", fmt.Sprintf(
				"no registered repo has uid %q; identify lens members by the uid from GET %s/repos",
				uid, APIBase), nil
		}
		return "", "", nil
	}
	if write != "" {
		if title, detail, err := known(write); detail != "" || err != nil {
			return title, detail, err
		}
	}
	for i, rd := range reads {
		if rd.UID == "" {
			return "Missing repo uid", fmt.Sprintf(
				`reads[%d] has no "uid"; identify lens members by the uid from GET %s/repos`,
				i, APIBase), nil
		}
		if title, detail, err := known(rd.UID); detail != "" || err != nil {
			return title, detail, err
		}
	}
	return "", "", nil
}

// rejectUnknownMembers writes the 400 (or 500) and reports whether the request
// should stop. Shared by create and patch so the two cannot drift.
func rejectUnknownMembers(w http.ResponseWriter, r *http.Request, reg *repos.Registry, write string, reads []lensReadDTO) bool {
	title, detail, err := checkLensMembers(reg, write, reads)
	if err != nil {
		log.Error().Err(err).Str("path", r.URL.Path).Msg("resolve lens member uids failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Lookup failed", "resolve lens members failed", r.URL.Path)
		return true
	}
	if detail != "" {
		hal.WriteProblem(w, http.StatusBadRequest, title, detail, r.URL.Path)
		return true
	}
	return false
}

// handleHALLenses serves GET /api/v1/lenses.
func handleHALLenses(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		lenses, err := reg.List()
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Msg("list lenses failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "List failed", "list lenses failed", r.URL.Path)
			return
		}
		views, err := lensViewsOf(b, m.Repos(), lenses)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Msg("resolve lens member names failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "List failed", "resolve lens member names failed", r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[lensView]{
			Count:    len(views),
			Links:    hal.LinkMap{"self": {Href: b.Lenses()}},
			Embedded: map[string][]lensView{"lenses": views},
		})
	}
}

// handleHALLens serves GET /api/v1/lenses/{lens}.
func handleHALLens(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		l, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		writeLensView(w, r, b, m.Repos(), http.StatusOK, l)
	}
}

// handleHALLensesCreate serves POST /api/v1/lenses. All validation runs inside
// Manager.CreateLens (name grammar, repo collision, replica, branch pins) — the
// handler never calls LensRegistry.Create directly.
func handleHALLensesCreate(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.LensRegistry() == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		var req createLensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		if rejectUnknownMembers(w, r, m.Repos(), req.Write.UID, req.Reads) {
			return
		}
		reads := make([]repos.LensRead, len(req.Reads))
		for i, rd := range req.Reads {
			reads[i] = repos.LensRead{RepoUID: rd.UID, Branch: rd.Branch, Source: rd.Source}
		}
		now := time.Now().Unix() // the caller stamps timestamps; the registry never reads the clock
		lens := repos.Lens{
			Name: req.Name, WriteUID: req.Write.UID, Description: req.Description, Reads: reads,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := m.CreateLens(r.Context(), lens)
		if err != nil {
			status, title := lensCreateErrStatus(err)
			detail := err.Error()
			// The mapped domain arms (400/409/422) carry clean, load-bearing
			// strings; only the 500 fall-through would leak a wrapped SQL/driver
			// error, so scrub it and log the real cause server-side.
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("path", r.URL.Path).Str("lens", req.Name).Msg("create lens failed")
				detail = "create lens failed"
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}
		writeLensView(w, r, b, m.Repos(), http.StatusCreated, created)
	}
}

// handleHALLensPatch serves PATCH /api/v1/lenses/{lens}. It edits a lens's write
// repo, read mounts, and description; the name is immutable. Omitted fields keep
// their current value, provided fields replace wholesale. The merge starts from
// the persisted lens (a 404 if unknown), then Manager.UpdateLens re-runs the full
// create-time validation (member existence, replica, branch pins, description
// cap) under the same locking discipline before persisting.
func handleHALLensPatch(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		current, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		var req patchLensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}

		// Start from the persisted lens; apply only the provided fields. created_at
		// is carried through unchanged; the caller stamps a fresh updated_at.
		lens := current
		lens.UpdatedAt = time.Now().Unix()
		// Only the PROVIDED members are checked: the fields left out keep uids
		// that were validated when they were written, and re-checking them would
		// turn an unrelated edit into a 400 the caller cannot act on.
		var providedWrite string
		var providedReads []lensReadDTO
		if req.Write != nil {
			lens.WriteUID = req.Write.UID
			providedWrite = req.Write.UID
		}
		if req.Description != nil {
			lens.Description = *req.Description
		}
		if req.Reads != nil {
			providedReads = *req.Reads
			reads := make([]repos.LensRead, len(*req.Reads))
			for i, rd := range *req.Reads {
				reads[i] = repos.LensRead{RepoUID: rd.UID, Branch: rd.Branch, Source: rd.Source}
			}
			lens.Reads = reads
		}
		if rejectUnknownMembers(w, r, m.Repos(), providedWrite, providedReads) {
			return
		}

		updated, err := m.UpdateLens(r.Context(), lens)
		if err != nil {
			status, title := lensPatchErrStatus(err)
			detail := err.Error()
			// As with create, only the 500 fall-through risks leaking a wrapped
			// SQL/driver error — scrub it and log the real cause server-side.
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("update lens failed")
				detail = "update lens failed"
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}
		writeLensView(w, r, b, m.Repos(), http.StatusOK, updated)
	}
}

// handleHALLensDelete serves DELETE /api/v1/lenses/{lens}. Get-check-then-Delete
// so an unknown lens is a 404 rather than a silent 204 (Delete is idempotent).
func handleHALLensDelete(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		_, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		if err := reg.Delete(name); err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("delete lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Delete failed", "delete lens failed", r.URL.Path)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// lensCreateErrStatus maps CreateLens validation sentinels to HTTP status +
// problem title, mirroring archiveErrStatus.
func lensCreateErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, repos.ErrInvalidLensName):
		return http.StatusBadRequest, "Invalid lens name"
	case errors.Is(err, repos.ErrLensNameConflictsRepo):
		return http.StatusConflict, "Lens name conflicts with a repo"
	case errors.Is(err, repos.ErrLensExists):
		return http.StatusConflict, "Lens already exists"
	case errors.Is(err, repos.ErrCreateInFlight):
		return http.StatusConflict, "Create in flight"
	case errors.Is(err, repos.ErrReplicaInLens):
		return http.StatusConflict, "Replica mounts not allowed"
	case errors.Is(err, repos.ErrRepoNotFound):
		return http.StatusUnprocessableEntity, "Lens references an unknown repo"
	case errors.Is(err, repos.ErrLensBranchUnknown):
		return http.StatusUnprocessableEntity, "Lens pins an unknown branch"
	case errors.Is(err, repos.ErrLensWriteEmpty):
		return http.StatusBadRequest, "Lens write repo required"
	case errors.Is(err, repos.ErrLensDescriptionTooLong):
		return http.StatusUnprocessableEntity, "Lens description too long"
	case errors.Is(err, repos.ErrLensNotFound):
		// Reached only via PATCH, when the lens is deleted between the handler's
		// Get and UpdateLens's persist. Create never produces it.
		return http.StatusNotFound, "Lens not found"
	default:
		return http.StatusInternalServerError, "Create lens failed"
	}
}

// lensPatchErrStatus reuses lensCreateErrStatus's sentinel→(status,title)
// mapping — the 4xx/422 validation arms are identical on the PATCH path — but
// relabels the scrubbed-500 default arm so the problem title names the actual
// operation ("Update lens failed" rather than "Create lens failed").
func lensPatchErrStatus(err error) (int, string) {
	status, title := lensCreateErrStatus(err)
	if status == http.StatusInternalServerError {
		title = "Update lens failed"
	}
	return status, title
}
