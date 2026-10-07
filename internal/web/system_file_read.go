package web

import (
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// systemFileReadMax bounds a raw system-file GET. A larger file is refused
// with 413 rather than streamed: .knomit/ holds configuration and templates,
// and a multi-megabyte blob there is not something a fact reader expects.
const systemFileReadMax = 4 << 20

// serveSystemFileRead serves a REST GET of a file under .knomit/ by its exact
// path (user ruling 2026-10-06: reads of .knomit/ by path are open, writes stay
// closed). It reports false — and writes nothing — when the captured path is
// not a well-formed system-file path, so the caller's private-path refusal
// still decides every other dot path, and every malformed .knomit/ one.
//
// The body is the file's raw bytes at commit (or the branch tip when commit is
// ""): text/plain; charset=utf-8 when it is UTF-8, application/octet-stream
// otherwise — never a HAL fact view, because it is not a fact. X-Knomit-Commit
// and X-Knomit-Blob say which version was served. There is no sub-resource:
// .knomit/x/commits names a FILE called commits.
//
// The path is judged percent-DECODED, as refusePrivateRead judges it, so an
// encoded ".." segment is malformed here and then refused there.
func serveSystemFileRead(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, branch, commit, captured string) bool {
	path := captured
	if dec, err := url.PathUnescape(captured); err == nil {
		path = dec
	}
	if !fact.IsSystemFilePath(path) {
		return false
	}
	writeSystemFile(w, r, ri, branch, commit, path, func() {
		at := `on branch "` + branch + `"`
		if commit != "" {
			at = `at commit ` + commit + ` on branch "` + branch + `"`
		}
		hal.WriteProblem(w, http.StatusNotFound, "System file not found",
			`no file at path "`+path+`" `+at+` (a system file is named by its exact path, case included)`, r.URL.Path)
	})
	return true
}

// serveLensSystemFileRead is serveSystemFileRead for the lens route (one read
// rule across REST, decision 2): the decoded path, bare (the write mount at
// its read branch) or kb://<id12>/.knomit/<path> (that mount), names a system
// file. Reports false when it does not, so refusePrivateRead still decides
// every other dot path. An unmounted id and a missing file both get the
// lens's one "Fact not found" 404 — no mount-topology oracle.
func serveLensSystemFileRead(w http.ResponseWriter, r *http.Request, bind *repos.Binding, requested string) bool {
	id, rel, qualified, err := federate.ParseQualifiedPath(requested)
	if err != nil || !fact.IsSystemFilePath(rel) {
		return false
	}
	ri, branch := bind.Write(), bind.WriteMountBranch()
	if qualified {
		rt, ok := bind.ByID(id)
		if !ok {
			lensFactNotFound(w, r, requested)
			return true
		}
		ri, branch = rt.RI, rt.Branch
	}
	writeSystemFile(w, r, ri, branch, "", rel, func() { lensFactNotFound(w, r, requested) })
	return true
}

// writeSystemFile reads path (a fact.IsSystemFilePath) in ri at commit (or the
// tip of branch) and writes the raw response; notFound writes the 404, so each
// route answers an absent file in its own shape.
func writeSystemFile(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, branch, commit, path string, notFound func()) {
	var (
		sf    store.SystemFile
		found bool
		err   error
	)
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		sf, found, err = svc.SystemFiles().SystemFileAt(r.Context(), branch, commit, path, systemFileReadMax)
	}); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		writeStoreError(w, r, err, "Failed to read system file", branch)
		return
	}
	if !found {
		notFound()
		return
	}
	if sf.Truncated {
		hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "System file too large",
			path+" is "+strconv.FormatInt(sf.Size, 10)+" bytes; the REST read serves at most "+
				strconv.Itoa(systemFileReadMax)+" bytes", r.URL.Path)
		return
	}
	ct := "application/octet-stream"
	if utf8.Valid(sf.Content) {
		ct = "text/plain; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Knomit-Commit", sf.Commit)
	h.Set("X-Knomit-Blob", sf.Blob)
	h.Set("Content-Length", strconv.Itoa(len(sf.Content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sf.Content)
}
