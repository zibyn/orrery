package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zibyn/stars-sandbox/internal/store"
)

type Server struct {
	cfg  Config
	pool *pgxpool.Pool
	q    *store.Queries
	log  *slog.Logger
}

func New(cfg Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{cfg: cfg, pool: pool, q: store.New(pool), log: log}
}

type principal struct {
	user  store.User
	token store.GetTokenWithUserRow
}

// isAdmin is the effective authority: an admin-scoped token only acts as admin
// while its User is still an admin.
func (p *principal) isAdmin() bool {
	return p.token.UserIsAdmin && p.token.Scope == "admin"
}

type handlerFunc func(http.ResponseWriter, *http.Request, *principal)

type route struct {
	Method        string
	Path          string // without the /api/v1 prefix
	Auth          string // "", "user", or "admin"
	OperationID   string
	Summary       string
	Request       string // schema name, or empty
	Response      string // schema name, or empty
	Query         []string
	SuccessStatus int
	Handler       handlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{http.MethodPost, "/users", "admin", "createUser", "Create a User", "CreateUserRequest", "User", nil, http.StatusCreated, s.handleCreateUser},
		{http.MethodGet, "/users", "admin", "listUsers", "List Users", "", "UserList", []string{"limit", "cursor"}, http.StatusOK, s.handleListUsers},
		{http.MethodGet, "/users/{id}", "admin", "getUser", "Get a User", "", "User", nil, http.StatusOK, s.handleGetUser},
		{http.MethodPatch, "/users/{id}", "admin", "updateUser", "Update a User", "UpdateUserRequest", "User", nil, http.StatusOK, s.handleUpdateUser},
		{http.MethodDelete, "/users/{id}", "admin", "deleteUser", "Delete a User", "", "User", nil, http.StatusAccepted, s.handleDeleteUser},
		{http.MethodPut, "/users/{id}/quota", "admin", "setUserQuota", "Set a User's quota", "SetQuotaRequest", "User", nil, http.StatusOK, s.handleSetUserQuota},
		{http.MethodGet, "/users/me", "user", "getCurrentUser", "Get the calling User", "", "User", nil, http.StatusOK, s.handleGetMe},
		{http.MethodGet, "/users/me/quota", "user", "getCurrentUserQuota", "Get the calling User's quota and usage", "", "UserQuotaUsage", nil, http.StatusOK, s.handleGetMyQuota},
		{http.MethodPost, "/users/me/tokens", "user", "createToken", "Create an API Token", "CreateTokenRequest", "CreatedToken", nil, http.StatusCreated, s.handleCreateToken},
		{http.MethodGet, "/users/me/tokens", "user", "listTokens", "List API Tokens", "", "TokenList", []string{"limit", "cursor"}, http.StatusOK, s.handleListTokens},
		{http.MethodDelete, "/users/me/tokens/{id}", "user", "revokeToken", "Revoke an API Token", "", "Token", nil, http.StatusAccepted, s.handleRevokeToken},
		{http.MethodPost, "/sessions", "", "createSession", "Exchange a password for a named API Token", "SessionRequest", "CreatedToken", nil, http.StatusCreated, s.handleCreateSession},
		{http.MethodGet, "/audit", "admin", "listAudit", "List Audit Records", "", "AuditList", []string{"actor", "action", "target", "target_type", "from", "to", "limit", "cursor"}, http.StatusOK, s.handleListAudit},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		h := rt.Handler
		var handler http.HandlerFunc
		switch rt.Auth {
		case "admin", "user":
			handler = s.wrapAuth(rt.Auth, h)
		default:
			handler = func(w http.ResponseWriter, r *http.Request) { h(w, r, nil) }
		}
		mux.HandleFunc(rt.Method+" /api/v1"+rt.Path, handler)
	}
	// Unknown paths and methods answer with problem+json; the closed code
	// catalog has no 405, so a wrong method is reported as not found.
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			writeProblem(w, "not_found", "no such endpoint")
			return
		}
		mux.ServeHTTP(w, r)
	})
	return s.withRequestLog(notFound)
}

func (s *Server) wrapAuth(level string, next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if level == "admin" && !p.isAdmin() {
			writeProblem(w, "permission_denied", "this endpoint requires an admin-scoped token")
			return
		}
		next(w, r, p)
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	raw := bearerToken(r)
	if raw == "" {
		writeUnauthenticated(w, "a Bearer API token is required")
		return nil, false
	}
	parts, ok := ParseToken(raw)
	if !ok {
		writeUnauthenticated(w, "malformed API token")
		return nil, false
	}
	ctx := r.Context()
	row, err := s.q.GetTokenWithUser(ctx, parts.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if err != nil {
		s.internal(w, "look up token", err)
		return nil, false
	}
	if subtle.ConstantTimeCompare(row.SecretHash, HashSecret(parts.Secret)) != 1 {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(time.Now()) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if row.Scope == "read_only" && r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, "permission_denied", "a read_only token may only make GET requests")
		return nil, false
	}
	user, err := s.q.GetUser(ctx, row.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if err != nil {
		s.internal(w, "look up token owner", err)
		return nil, false
	}
	// last_used_at is observability, not authorization; a failed write must not
	// fail the request.
	if err := s.q.TouchToken(ctx, row.ID); err != nil {
		s.log.Warn("touch token failed", "token_id", row.ID, "error", err)
	}
	return &principal{user: user, token: row}, true
}

func bearerToken(r *http.Request) string {
	scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func writeUnauthenticated(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, "unauthenticated", detail)
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "error", err)
	writeProblem(w, "internal", "")
}

func (s *Server) Serve(ctx context.Context) error {
	go s.purgeIdempotencyKeys(ctx)
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// purgeIdempotencyKeys enforces the 24-hour retention in api.md §1.
func (s *Server) purgeIdempotencyKeys(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := s.q.PurgeUserIdempotencyKeys(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("purge User idempotency keys", "error", err)
		}
		if err := s.q.PurgeTokenIdempotencyKeys(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("purge token idempotency keys", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type correlationKey struct{}

func correlationID(ctx context.Context) *string {
	id, _ := ctx.Value(correlationKey{}).(string)
	if id == "" {
		return nil
	}
	return &id
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status, w.wroteHeader = status, true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			if v7, err := uuid.NewV7(); err == nil {
				id = v7.String()
			} else {
				id = uuid.NewString()
			}
		}
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), correlationKey{}, id))

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("request panic", "panic", rec, "request_id", id)
				if !sw.wroteHeader {
					writeProblem(sw, "internal", "")
				}
			}
			s.log.Info("request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(sw, r)
	})
}
