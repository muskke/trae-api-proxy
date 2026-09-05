package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/handler"
	"github.com/muskke/trae-api-proxy/internal/middleware"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	traeClient := trae.NewClient(cfg)
	defer traeClient.CloseIdleConnections()
	h := handler.NewAPIHandler(cfg, traeClient)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.HandleRoot)
	mux.HandleFunc("GET /healthz", h.HandleHealth)
	mux.HandleFunc("GET /status", h.HandleStatus)
	mux.HandleFunc("GET /v1/models", h.HandleModels)
	mux.HandleFunc("POST /v1/chat/completions", h.HandleChatCompletions)

	wrapped := middleware.Chain(
		mux,
		middleware.RequestID,
		middleware.SecurityHeaders,
		middleware.CORS(cfg.CORSAllowOrigin),
		middleware.Recovery,
		middleware.Logger,
	)

	srv := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("trae-api-proxy listening on %s upstream_mode=%s upstream=%s", srv.Addr, cfg.UpstreamMode, cfg.APIBaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
		_ = srv.Close()
	}
}
