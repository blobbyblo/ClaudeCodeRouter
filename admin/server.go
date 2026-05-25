package admin

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/bobbyo/ccr/config"
	"github.com/bobbyo/ccr/middleware"
)

//go:embed static
var staticFiles embed.FS

// Server is the admin HTTP server bound to localhost only.
type Server struct {
	cfg      *config.Manager
	database *sql.DB
	bc       *middleware.Broadcaster
	cfgPath  string
	version  string

	cfgSubsMu sync.Mutex
	cfgSubs   map[chan struct{}]struct{}
}

// New creates an admin Server.
func New(cfg *config.Manager, database *sql.DB, bc *middleware.Broadcaster, cfgPath string) *Server {
	return &Server{
		cfg:      cfg,
		database: database,
		bc:       bc,
		cfgPath:  cfgPath,
		cfgSubs:  make(map[chan struct{}]struct{}),
	}
}

// SetVersion sets the version string exposed by /admin/api/version.
func (s *Server) SetVersion(v string) { s.version = v }

// SetCommit sets the commit hash (not yet used in UI, reserved for future).
func (s *Server) SetCommit(c string) {}

// NotifyConfigReload is called by the config manager after each successful
// hot-reload. It fans out a signal to all connected config-event SSE clients.
func (s *Server) NotifyConfigReload() {
	s.cfgSubsMu.Lock()
	defer s.cfgSubsMu.Unlock()
	for ch := range s.cfgSubs {
		select {
		case ch <- struct{}{}:
		default: // subscriber too slow — skip
		}
	}
}

func (s *Server) subscribeCfg() chan struct{} {
	ch := make(chan struct{}, 4)
	s.cfgSubsMu.Lock()
	s.cfgSubs[ch] = struct{}{}
	s.cfgSubsMu.Unlock()
	return ch
}

func (s *Server) unsubscribeCfg(ch chan struct{}) {
	s.cfgSubsMu.Lock()
	delete(s.cfgSubs, ch)
	s.cfgSubsMu.Unlock()
	close(ch)
}

// Start starts the admin server on the configured admin port and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context, port int) error {
	r := chi.NewRouter()

	// Reject non-loopback connections at the handler level.
	r.Use(loopbackOnly)

	// Static files.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("admin: embed static: %w", err)
	}
	r.Handle("/*", http.FileServer(http.FS(staticFS)))

	// API routes.
	r.Route("/admin/api", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.Get("/info", s.handleInfo)
		r.Get("/version", s.handleVersion)
		r.Post("/update", s.handleUpdate)
		r.Post("/test", s.handleTest)
		r.Post("/restart", s.handleRestart)

		r.Get("/keys", s.handleListKeys)
		r.Post("/keys", s.handleMintKey)
		r.Delete("/keys/{id}", s.handleRevokeKey)

		r.Get("/usage", s.handleUsage)
		r.Get("/usage/summary", s.handleUsageSummary)
		r.Get("/usage/live", s.handleUsageLive)

		r.Get("/models", s.handleListModels)
		r.Post("/models", s.handleUpsertModel)
		r.Delete("/models/{alias}", s.handleDeleteModel)

		r.Get("/providers", s.handleListProviders)
		r.Post("/providers", s.handleUpsertProvider)
		r.Delete("/providers/{id}", s.handleDeleteProvider)
		r.Get("/providers/{id}/keys", s.handleListProviderKeys)
		r.Post("/providers/{id}/keys", s.handleAddProviderKey)
		r.Delete("/providers/{id}/keys/{keyId}", s.handleDeleteProviderKey)

		r.Get("/config/path", s.handleConfigPath)
		r.Get("/config/raw", s.handleGetRawConfig)
		r.Post("/config/raw", s.handleSetRawConfig)
		r.Get("/config/events", s.handleConfigEvents)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: r}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("admin: listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

// loopbackOnly rejects any connection whose remote address is not loopback.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			// If we can't parse, be safe and reject.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			// Check X-Forwarded-For only when behind a local reverse proxy.
			xff := r.Header.Get("X-Forwarded-For")
			if xff == "" {
				http.Error(w, "admin panel: loopback only", http.StatusForbidden)
				return
			}
			// Take the first entry from X-Forwarded-For.
			firstIP := net.ParseIP(strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]))
			if firstIP == nil || !firstIP.IsLoopback() {
				http.Error(w, "admin panel: loopback only", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
