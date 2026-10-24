package jobs

import (
	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/llm"
)

// Mount registers the job routes on r, which the caller mounts at
// /repos/{repo}/branches/{branch} INSIDE the RepoMiddleware and BranchMiddleware
// groups: every handler reads the repo with repos.RepoFromContext, and the
// rebuild start reads the branch with httpx.BranchFromContext. rm is the repo
// manager the rebuild start and the rebuild cancel send lifecycle events
// through, and llmAdapter the model the synthesis start runs with (nil makes
// the synthesis start answer 503).
func Mount(r chi.Router, rm *repos.Manager, llmAdapter llm.LLMAdapter) {
	b := hal.URLBuilder{Base: httpx.APIBase}

	r.Post("/synthesis-runs", handleStartSynthesis(llmAdapter))
	r.Get("/synthesis-runs", handleListJobs("synthesis-run"))
	r.Get("/synthesis-runs/{id}", handleGetJob("synthesis-run"))
	r.Delete("/synthesis-runs/{id}", handleDeleteJob(b, rm, "synthesis-run"))
	r.Get("/synthesis-runs/{id}/events", handleJobEvents("synthesis-run"))

	r.Post("/index-rebuilds", handleStartRebuild(b, rm))
	r.Get("/index-rebuilds", handleListJobs("index-rebuild"))
	r.Get("/index-rebuilds/{id}", handleGetJob("index-rebuild"))
	r.Delete("/index-rebuilds/{id}", handleDeleteJob(b, rm, "index-rebuild"))
	r.Get("/index-rebuilds/{id}/events", handleJobEvents("index-rebuild"))
}
