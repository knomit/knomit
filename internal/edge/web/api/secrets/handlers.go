package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

const maxSecretBody = 64 << 10

const unavailableDetail = "Secret storage is unavailable: the instance key cannot be read."

type secretView struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Hosts       []string       `json:"hosts"`
	ExpiresAt   *string        `json:"expires_at"`
	Status      string         `json:"status"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	LinkedRepos int            `json:"linked_repos"`
	Repos       *[]repoRef     `json:"repos,omitempty"` // detail view only
	Links       map[string]any `json:"_links"`
}

type repoRef struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	State string `json:"state"`
}

func selfHref(id string) string { return httpx.APIBase + "/secrets/" + id }

func viewOf(m control.SecretMeta, now time.Time) secretView {
	v := secretView{
		ID: m.ID, Name: m.Name, Type: m.Type, Hosts: m.Hosts,
		Status:      control.ExpiryStatus(m.ExpiresAt, now),
		CreatedAt:   m.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   m.UpdatedAt.UTC().Format(time.RFC3339),
		Fingerprint: m.Fingerprint,
		LinkedRepos: m.LinkedRepos,
		Links:       map[string]any{"self": map[string]string{"href": selfHref(m.ID)}},
	}
	if v.Hosts == nil {
		v.Hosts = []string{}
	}
	if m.ExpiresAt != nil {
		s := m.ExpiresAt.UTC().Format(time.RFC3339)
		v.ExpiresAt = &s
	}
	return v
}

func refsOf(rs []control.LinkedRepo) []repoRef {
	out := make([]repoRef, 0, len(rs))
	for _, r := range rs {
		out = append(out, repoRef{Name: r.Name, UID: r.UID, State: r.State})
	}
	return out
}

func names(rs []control.LinkedRepo) string {
	n := make([]string, len(rs))
	for i, r := range rs {
		n[i] = r.Name
	}
	return strings.Join(n, ", ")
}

// writeError maps a secrets error to its problem document. Errors name a
// secret by name or field, never by value.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *control.InUseError
	var hostInUse *control.HostInUseError
	switch {
	case errors.As(err, &hostInUse):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Conflict",
			fmt.Sprintf("Host(s) %s are used by: %s. Unlink those repositories or change their URL first.",
				strings.Join(hostInUse.Hosts, ", "), names(hostInUse.Repos)),
			r.URL.Path, map[string]any{"hosts": hostInUse.Hosts, "repos": refsOf(hostInUse.Repos)})
	case errors.As(err, &inUse):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Conflict",
			"Unlink these repositories first: "+names(inUse.Repos),
			r.URL.Path, map[string]any{"repos": refsOf(inUse.Repos)})
	case errors.Is(err, control.ErrSecretNotFound):
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no secret with this id", r.URL.Path)
	case errors.Is(err, control.ErrSecretNameTaken):
		hal.WriteProblem(w, http.StatusConflict, "Conflict", "a secret with this name already exists: choose another name", r.URL.Path)
	case errors.Is(err, control.ErrSecretInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error(), r.URL.Path)
	case errors.Is(err, control.ErrSecretUndecryptable):
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error(), r.URL.Path)
	case errors.Is(err, control.ErrEncryptionUnavailable):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", unavailableDetail, r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error(), r.URL.Path)
	}
}

// store returns the secrets tenant, or answers 503 when it is not open.
func store(w http.ResponseWriter, r *http.Request, rm *repos.Manager) *control.Secrets {
	if rm == nil || rm.Secrets() == nil {
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", unavailableDetail, r.URL.Path)
		return nil
	}
	return rm.Secrets()
}

func handleListSecrets(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store(w, r, rm)
		if s == nil {
			return
		}
		list, err := s.List()
		if err != nil {
			writeError(w, r, err)
			return
		}
		now := time.Now().UTC()
		out := make([]secretView, 0, len(list))
		for _, m := range list {
			out = append(out, viewOf(m, now))
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"secrets": out,
			"_links":  map[string]any{"self": map[string]string{"href": httpx.APIBase + "/secrets"}},
		})
	}
}

func handleGetSecret(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store(w, r, rm)
		if s == nil {
			return
		}
		writeDetail(w, r, s, chi.URLParam(r, "id"), http.StatusOK)
	}
}

// writeDetail answers with one secret's view plus its linked repos.
func writeDetail(w http.ResponseWriter, r *http.Request, s *control.Secrets, id string, status int) {
	m, err := s.Get(id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	linked, err := s.LinkedRepos(id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	v := viewOf(m, time.Now().UTC())
	refs := refsOf(linked)
	v.Repos = &refs
	hal.WriteHAL(w, status, v)
}

// valueFields are the write-only value inputs, mapped onto the type:
// value is the token, username and password the basic pair, private_key and
// passphrase the SSH key.
type valueFields struct {
	Value      *string `json:"value"`
	Username   *string `json:"username"`
	Password   *string `json:"password"`
	PrivateKey *string `json:"private_key"`
	Passphrase *string `json:"passphrase"`
}

func (f valueFields) present() bool {
	return f.Value != nil || f.Username != nil || f.Password != nil || f.PrivateKey != nil || f.Passphrase != nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (f valueFields) secretValue() control.SecretValue {
	return control.SecretValue{
		Token: deref(f.Value), Username: deref(f.Username), Password: deref(f.Password),
		PrivateKey: deref(f.PrivateKey), Passphrase: deref(f.Passphrase),
	}
}

// parseExpiry reads an RFC 3339 expires_at; "" is none.
func parseExpiry(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("%w: expires_at: must be an RFC 3339 time such as 2026-12-31T00:00:00Z", control.ErrSecretInvalid)
	}
	t = t.UTC()
	return &t, nil
}

func handleCreateSecret(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store(w, r, rm)
		if s == nil {
			return
		}
		var body struct {
			Name      string   `json:"name"`
			Type      string   `json:"type"`
			Hosts     []string `json:"hosts"`
			ExpiresAt *string  `json:"expires_at"`
			valueFields
		}
		if !httpx.DecodeJSON(w, r, &body, maxSecretBody) {
			return
		}
		exp, err := parseExpiry(deref(body.ExpiresAt))
		if err != nil {
			writeError(w, r, err)
			return
		}
		m, err := s.Create(control.SecretInput{
			Name: body.Name, Type: body.Type, Hosts: body.Hosts, ExpiresAt: exp,
			Value: body.secretValue(),
		}, time.Now().UTC())
		if err != nil {
			writeError(w, r, err)
			return
		}
		w.Header().Set("Location", selfHref(m.ID))
		hal.WriteHAL(w, http.StatusCreated, viewOf(m, time.Now().UTC()))
	}
}

// handlePatchSecret validates every input before writing anything, then
// applies name, hosts and expiry in ONE transaction (Secrets.Update), so any
// refusal, a host in use or a taken name included, changes nothing. The
// value, already validated, is replaced last.
func handlePatchSecret(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store(w, r, rm)
		if s == nil {
			return
		}
		id := chi.URLParam(r, "id")
		var body struct {
			Name      *string         `json:"name"`
			Hosts     *[]string       `json:"hosts"`
			ExpiresAt json.RawMessage `json:"expires_at"`
			valueFields
		}
		if !httpx.DecodeJSON(w, r, &body, maxSecretBody) {
			return
		}
		meta, err := s.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		// expires_at: absent keeps it, null clears it, a string sets it.
		setExpiry := len(body.ExpiresAt) > 0
		var exp *time.Time
		if setExpiry && string(body.ExpiresAt) != "null" {
			var raw string
			if err := json.Unmarshal(body.ExpiresAt, &raw); err != nil {
				writeError(w, r, fmt.Errorf("%w: expires_at: must be an RFC 3339 string or null", control.ErrSecretInvalid))
				return
			}
			if exp, err = parseExpiry(raw); err != nil {
				writeError(w, r, err)
				return
			}
		}
		if body.Name != nil {
			if err := control.ValidateSecretName(*body.Name); err != nil {
				writeError(w, r, err)
				return
			}
		}
		if body.Hosts != nil {
			if _, err := control.NormalizeHosts(*body.Hosts); err != nil {
				writeError(w, r, err)
				return
			}
		}
		if body.present() {
			if err := control.ValidateSecretValue(meta.Type, body.secretValue()); err != nil {
				writeError(w, r, err)
				return
			}
		}

		// Name, hosts and expiry in one transaction: any refusal writes none.
		if err := s.Update(id, control.SecretUpdate{
			Name: body.Name, Hosts: body.Hosts, SetExpiry: setExpiry, ExpiresAt: exp,
		}, time.Now().UTC()); err != nil {
			writeError(w, r, err)
			return
		}
		if body.present() {
			if err := rm.ReplaceSecretValue(id, body.secretValue()); err != nil {
				writeError(w, r, err)
				return
			}
		}
		m, err := s.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, viewOf(m, time.Now().UTC()))
	}
}

func handleDeleteSecret(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store(w, r, rm)
		if s == nil {
			return
		}
		if err := s.Delete(chi.URLParam(r, "id")); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
