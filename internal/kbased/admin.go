package kbased

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bio4554/lab/internal/kbclient"
)

// The admin API is how principals and tokens come to exist; labd is
// the caller (it registers agent principals and mints their
// project-scoped tokens at container create), and a human mints their
// own token with curl once.
//
// Auth decision (recorded per the Phase 9 handoff): a
// config-designated admin token, not a separate localhost-only
// listener. On Docker Desktop host.docker.internal reaches services
// bound to the host's 127.0.0.1, so a localhost listener would not
// actually keep agent containers out; a shared secret in lab.toml
// (never in the database, never in the repo) does. An empty
// admin_token disables the admin API outright.

// adminMux returns the /admin/v1 routes (unauthenticated; wrap with
// adminOnly).
func (s *Server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/principals", s.registerPrincipal)
	mux.HandleFunc("POST /admin/v1/tokens", s.mintToken)
	mux.HandleFunc("POST /admin/v1/tokens/revoke", s.revokeTokens)
	return mux
}

// adminOnly gates the admin routes on the configured admin token.
func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			s.writeError(w, http.StatusForbidden,
				errors.New("admin API disabled: no admin_token configured"))
			return
		}
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(secret), []byte(s.AdminToken)) != 1 {
			s.writeError(w, http.StatusUnauthorized, errors.New("invalid admin token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerPrincipal handles POST /admin/v1/principals: upsert on
// (kind, external_id).
func (s *Server) registerPrincipal(w http.ResponseWriter, r *http.Request) {
	var req kbclient.RegisterPrincipalRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Kind != "agent" && req.Kind != "human" {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("unknown kind %q (agent|human)", req.Kind))
		return
	}
	if req.ExternalID == "" {
		s.writeError(w, http.StatusBadRequest, errors.New("external_id is required"))
		return
	}
	p, err := s.Store.EnsurePrincipal(r.Context(), req.Kind, req.ExternalID, req.DisplayName)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, p)
}

// mintToken handles POST /admin/v1/tokens. The response is the only
// place the plaintext secret ever appears.
func (s *Server) mintToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req kbclient.MintTokenRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.Store.GetPrincipal(ctx, req.PrincipalID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if req.RevokeExisting {
		if _, err := s.Store.RevokeTokens(ctx, req.PrincipalID, req.ProjectID); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	resp, err := s.Store.MintToken(ctx, req.PrincipalID, req.ProjectID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, resp)
}

// revokeTokens handles POST /admin/v1/tokens/revoke: revokes the
// principal's active tokens with exactly the given scope (nil project
// = the scope-less tokens).
func (s *Server) revokeTokens(w http.ResponseWriter, r *http.Request) {
	var req kbclient.RevokeTokensRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.Store.RevokeTokens(r.Context(), req.PrincipalID, req.ProjectID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, kbclient.RevokeTokensResponse{Revoked: n})
}
