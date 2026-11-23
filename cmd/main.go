package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"openai-proxy/internal/config"
	"openai-proxy/internal/logging"
	"openai-proxy/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	flag.Parse()

	bootstrapLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		bootstrapLogger.Error("load configuration failed", "error", err)
		os.Exit(1)
	}
	logger, logCloser, err := logging.New(cfg.Logging)
	if err != nil {
		bootstrapLogger.Error("initialize logging failed", "path", cfg.Logging.Path, "error", err)
		os.Exit(1)
	}
	defer logCloser.Close()
	handler, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("initialize server failed", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:           net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
		Handler:        handler.Handler(),
		ReadTimeout:    cfg.Server.ReadTimeout.Duration(),
		WriteTimeout:   cfg.Server.WriteTimeout.Duration(),
		IdleTimeout:    cfg.Server.IdleTimeout.Duration(),
		MaxHeaderBytes: 1 << 20,
	}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("openai proxy listening", "address", httpServer.Addr, "upstream", cfg.Upstream.BaseURL, "keys", len(cfg.Keys))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
