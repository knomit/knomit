package web

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// triggersMaxLog caps `?log=N`, the number of recent fire-log rows returned.
const triggersMaxLog = 500

// triggersView is the HAL body of GET …/branches/{branch}/triggers: each
// declared trigger with its state, error, watermark and statistics, the run
// statistics, and the last `log` fire rows. It is the ONLY place the trigger
// statistics are exposed (user ruling D-d).
type triggersView struct {
	Enabled  bool                  `json:"enabled"`
	Reason   string                `json:"reason,omitempty"`
	Branch   string                `json:"branch"`
	Head     string                `json:"head,omitempty"`
	Blob     string                `json:"ontology_blob,omitempty"`
	Runs     repos.TriggerRunStats `json:"runs"`
	Triggers []repos.TriggerView   `json:"triggers"`
	Fires    []store.TriggerFire   `json:"fires"`
	Links    hal.LinkMap           `json:"_links"`
}

// handleHALTriggers serves GET /repos/{repo}/branches/{branch}/triggers. The
// dispatcher observes the repo's agent branch, so the report is the same
// whichever branch is in the URL; `branch` in the body names the observed one.
func handleHALTriggers(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())

		logN := 0
		if v := r.URL.Query().Get("log"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > triggersMaxLog {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"log must be an integer from 0 to "+strconv.Itoa(triggersMaxLog), r.URL.Path)
				return
			}
			logN = n
		}

		rep, err := ri.TriggerReport(r.Context(), logN)
		if err != nil {
			writeStoreError(w, r, err, "Failed to read triggers", branch)
			return
		}
		self := b.Branch(repoName, hal.Anchor{Branch: branch}) + "/triggers"
		hal.WriteHAL(w, http.StatusOK, triggersView{
			Enabled:  rep.Enabled,
			Reason:   rep.Reason,
			Branch:   rep.Branch,
			Head:     rep.Head,
			Blob:     rep.Blob,
			Runs:     rep.Runs,
			Triggers: rep.Triggers,
			Fires:    rep.Fires,
			Links: hal.LinkMap{
				"self":   {Href: selfWithQuery(self, r)},
				"events": {Href: b.Branch(repoName, hal.Anchor{Branch: branch}) + "/events"},
			},
		})
	}
}
