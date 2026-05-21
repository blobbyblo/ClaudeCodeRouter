package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/bobbyo/ccr/admin"
	"github.com/bobbyo/ccr/config"
	"github.com/bobbyo/ccr/db"
	"github.com/bobbyo/ccr/middleware"
	"github.com/bobbyo/ccr/router"
)

func main() {
	var version = "cc-router v0.1.0"
	cfgPath := flag.String("config", "config.toml", "path to config.toml")
	dbPath  := flag.String("db", "ccr.db", "path to SQLite database")
	host    := flag.String("host", "127.0.0.1", "host address for the client-facing server (use 0.0.0.0 to expose on all interfaces)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}



	// ---- Config ----
	if err := config.GenerateIfMissing(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	cfgMgr, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}

	// ---- Database ----
	database, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	// ---- Logging ----
	cfg := cfgMgr.Get()
	level := slog.LevelInfo
	if cfg.Server.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// ---- Live log broadcaster ----
	bc := middleware.NewBroadcaster()

	// ---- Router ----
	rt := router.New(cfgMgr, database, bc)

	// ---- Admin server ----
	adminSrv := admin.New(cfgMgr, database, bc, *cfgPath)
	cfgMgr.SetReloadCallback(adminSrv.NotifyConfigReload)

	// ---- Context / signal handling ----
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ---- Hot-reload watcher ----
	go func() {
		if err := cfgMgr.Watch(ctx); err != nil && ctx.Err() == nil {
			slog.Error("config watcher stopped", "err", err)
		}
	}()

	// ---- Admin server goroutine ----
	go func() {
		if err := adminSrv.Start(ctx, cfgMgr.Get().Server.AdminPort); err != nil && ctx.Err() == nil {
			slog.Error("admin server error", "err", err)
			cancel()
		}
	}()

	// ---- Client-facing server ----
	r := chi.NewRouter()
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	// Auth middleware protects proxy endpoints.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Auth(database))
		r.Post("/v1/messages", rt.HandleMessages)
		r.Post("/v1/chat/completions", rt.HandleChatCompletions)
		r.Get("/v1/models", rt.HandleModels)
	})

	clientAddr := fmt.Sprintf("%s:%d", *host, cfgMgr.Get().Server.ClientPort)
	clientSrv := &http.Server{Addr: clientAddr, Handler: r}

	go func() {
		<-ctx.Done()
		_ = clientSrv.Shutdown(context.Background())
	}()

	slog.Info("client server listening", "addr", clientAddr)
	if err := clientSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("client server error", "err", err)
		os.Exit(1)
	}
}
