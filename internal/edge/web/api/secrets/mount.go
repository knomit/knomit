// Package secrets serves the instance's stored credentials under /secrets:
// list, create, read, change (name, expiry, hosts, value) and delete. Values
// are write-only: no response ever carries one. Its only export is Mount.
package secrets

import (
	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
)

// Mount registers the secrets routes on r, which the caller mounts at
// /secrets BEHIND the local-or-same-origin admin gate: nothing here checks
// the caller.
func Mount(r chi.Router, rm *repos.Manager) {
	r.Get("/", handleListSecrets(rm))
	r.Post("/", handleCreateSecret(rm))
	r.Get("/{id}", handleGetSecret(rm))
	r.Patch("/{id}", handlePatchSecret(rm))
	r.Delete("/{id}", handleDeleteSecret(rm))
}
