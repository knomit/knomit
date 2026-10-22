package store

import (
	"net/http"

	"knomit/internal/engine/store/gitserve"
)

// Handler returns an http.Handler implementing the Smart HTTP git protocol
// (https://git-scm.com/docs/http-protocol) for this store: upload-pack
// (clone/fetch) for anyone the edge let in, and receive-pack (push) for an
// enrolled peer updating its own agent branch only (F11). The handler is
// built lazily and cached; everything per-request — above all who is
// pushing — arrives on the request context (WithPushPolicy).
//
//   - GET  /info/refs?service=git-upload-pack  — advertise refs
//   - POST /git-upload-pack                    — serve a fetch
//   - GET  /info/refs?service=git-receive-pack — advertise refs for a push
//   - POST /git-receive-pack                   — accept a push
func (s *Service) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		s.handler = gitserve.NewHandler(s.rh.Repo, s.rr, s.ri.UpstreamBranch)
	})
	return s.handler
}
