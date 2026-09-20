// Command minemanager is a self-hosted Minecraft server manager.
//
// It provisions and runs Vanilla / Paper / Purpur / Fabric / Forge servers,
// exposes a console, RCON bridge, backups, scheduling, crash detection with
// auto-restart, and serves a built-in Web UI on http://127.0.0.1:8080.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"minemanager/internal/config"
	"minemanager/internal/manager"
	"minemanager/internal/web"
)

func main() {
	var (
		dataDir = flag.String("data", "./data", "data directory (servers, backups, config)")
		addr    = flag.String("addr", "127.0.0.1:8080", "HTTP listen address for the Web UI")
		debug   = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(*dataDir)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	cfg.DataDir = *dataDir
	cfg.Host = *addr
	if cfg.UserAgent == "" {
		// PaperMC's downloads service rejects generic user agents, so advertise
		// this manager and where it is operated from. Overridable in config.json.
		cfg.UserAgent = fmt.Sprintf("MinecraftServerManager/1.0 (self-hosted manager at http://%s)", cfg.Host)
	}
	if err := cfg.Save(); err != nil {
		slog.Warn("failed to save config", "err", err)
	}

	mgr, err := manager.New(cfg)
	if err != nil {
		slog.Error("failed to initialise manager", "err", err)
		os.Exit(1)
	}
	defer mgr.Close()

	srv := &http.Server{
		Addr:              cfg.Host,
		Handler:           web.New(mgr),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("starting Minecraft Manager", "addr", "http://"+cfg.Host)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown failed", "err", err)
	}
}
