package api

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/zibyn/orrery/internal/objectstore"
	"github.com/zibyn/orrery/internal/store"
)

type Server struct {
	cfg          Config
	pool         *pgxpool.Pool
	q            *store.Queries
	log          *slog.Logger
	objects      *objectstore.Store // nil when object storage is unconfigured
	logins       *loginLimiter
	crossOrigin  http.CrossOriginProtection
	reconcile    *reconciler
	tunnels      *tunnelRegistry
	activity     *activityTracker
	oidcMu       sync.Mutex
	oidcProvider *oidc.Provider
	// snapshotCancels ends in-flight imports (api.md §3). Imports do not
	// survive a restart, so an in-process map is the whole registry.
	snapshotMu      sync.Mutex
	snapshotCancels map[uuid.UUID]context.CancelFunc
}

func New(cfg Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	if len(cfg.PortTicketKey) == 0 {
		cfg.PortTicketKey = make([]byte, 32)
		if _, err := rand.Read(cfg.PortTicketKey); err != nil {
			panic(err) // no entropy, no serving
		}
	}
	s := &Server{cfg: cfg, pool: pool, q: store.New(pool), log: log, logins: newLoginLimiter(),
		tunnels: newTunnelRegistry(), activity: newActivityTracker(),
		snapshotCancels: map[uuid.UUID]context.CancelFunc{}}
	if cfg.S3.Endpoint != "" {
		objects, err := objectstore.New(context.Background(), objectstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			PublicEndpoint: cfg.S3.PublicEndpoint,
			Bucket:         cfg.S3.Bucket,
			Region:         cfg.S3.Region,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
		})
		if err != nil {
			log.Error("build object store client", "error", err)
		} else {
			s.objects = objects
		}
	}
	s.reconcile = newReconciler(s)
	return s
}

func (s *Server) Serve(ctx context.Context) error {
	go s.purgeExpired(ctx)
	go s.reconcile.run(ctx)
	// An import runs inside this process; one left building by a previous
	// instance is failed so a Sandbox create does not wait on it forever.
	if err := s.q.FailBuildingSnapshotImports(ctx); err != nil {
		s.log.Error("fail interrupted Snapshot imports", "error", err)
	}
	go s.importPlatformSnapshots(ctx)
	httpServer := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	grpcServer, nodeListener, err := s.nodeServer()
	if err != nil {
		return err
	}
	errCh := make(chan error, 2)
	go func() { errCh <- httpServer.ListenAndServe() }()
	go func() { errCh <- grpcServer.Serve(nodeListener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Abrupt Stop ends long-lived Control streams; Agents reconnect.
		grpcServer.Stop()
		_ = nodeListener.Close()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		grpcServer.Stop()
		_ = httpServer.Close()
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "error", err)
	writeProblem(w, "internal", "")
}
