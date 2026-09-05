package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	traeauth "github.com/muskke/trae-api-proxy/internal/auth"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authManager := traeauth.NewManager(cfg)
	defer authManager.CloseIdleConnections()
	go authManager.Run(ctx)

	traeClient := trae.NewClient(cfg)
	defer traeClient.CloseIdleConnections()
	h := handler.NewAPIHandler(cfg, traeClient, authManager)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.HandleRoot)
	mux.HandleFunc("GET /healthz", h.HandleHealth)
	mux.HandleFunc("GET /status", h.HandleStatus)
	mux.HandleFunc("GET /auth/status", h.HandleAuthStatus)
	mux.HandleFunc("GET /auth/login", h.HandleAuthLogin)
	mux.HandleFunc("GET /auth/callback/{state}", h.HandleAuthCallback)
	mux.HandleFunc("GET /authorize", h.HandleAuthCallback)
	mux.HandleFunc("POST /auth/refresh", h.HandleAuthRefresh)
	mux.HandleFunc("POST /auth/logout", h.HandleAuthLogout)
	mux.HandleFunc("GET /v1/models", h.HandleModels)
	mux.HandleFunc("GET /v1/models/status", h.HandleModelStatus)
	mux.HandleFunc("DELETE /v1/models/status", h.HandleModelStatusReset)
	mux.HandleFunc("POST /v1/chat/completions", h.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", h.HandleResponses)

	wrapped := middleware.Chain(
		mux,
		middleware.RequestID,
		middleware.SecurityHeaders,
		middleware.CORS(cfg.CORSAllowOrigin),
		middleware.Recovery,
		middleware.Logger,
	)

	srv := newHTTPServer(cfg.ListenAddr(), wrapped)

	var callbackSrv *http.Server
	if callbackAddr, localCallback := cfg.OAuthCallbackListenAddr(); localCallback && !cfg.PrimaryServesOAuthCallback() {
		callbackMux := http.NewServeMux()
		callbackMux.HandleFunc("GET /auth/callback/{state}", h.HandleAuthCallback)
		callbackMux.HandleFunc("GET /authorize", h.HandleAuthCallback)
		callbackHandler := middleware.Chain(
			callbackMux,
			middleware.RequestID,
			middleware.SecurityHeaders,
			middleware.Recovery,
			middleware.Logger,
		)
		callbackSrv = newHTTPServer(callbackAddr, callbackHandler)
		go func() {
			log.Printf("TRAE OAuth callback listening on %s", callbackAddr)
			if err := callbackSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("OAuth callback listen: %v", err)
			}
		}()
	}

	go func() {
		authStatus := authManager.Status()
		platform := authStatus.Platform
		if platform == "" {
			platform = cfg.OAuthPlatform
		}
		chatUpstream := authStatus.APIBaseURL
		if chatUpstream == "" {
			chatUpstream = cfg.APIBaseURL
		}
		oauthHost := authStatus.OAuthHost
		if oauthHost == "" {
			oauthHost = cfg.OAuthHost
		}
		function := traeClient.FunctionFor(trae.Session{Platform: platform}, cfg.DefaultModel)
		log.Printf("trae-api-proxy listening on %s upstream_mode=%s auth_mode=%s auth_source=%s platform=%s region=%s function=%s chat_upstream=%s oauth_host=%s reasoning=%s model_status_file=%s", srv.Addr, cfg.UpstreamMode, cfg.AuthMode, authStatus.Source, platform, authStatus.LoginRegion, function, chatUpstream, oauthHost, cfg.ReasoningMode, cfg.ModelStatusFile)
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
	if callbackSrv != nil {
		if err := callbackSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("OAuth callback shutdown failed: %v", err)
			_ = callbackSrv.Close()
		}
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
