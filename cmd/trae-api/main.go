package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/handler"
	"github.com/muskke/trae-api-proxy/internal/middleware"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

func main() {
	// 1. Load Configuration
	cfg := config.Load()

	// 2. Initialize Services
	traeClient := trae.NewClient(cfg)

	// 3. Initialize Handlers
	h := handler.NewAPIHandler(cfg, traeClient)

	// 4. Setup Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", h.HandleModels)
	mux.HandleFunc("/v1/chat/completions", h.HandleChatCompletions)

	// 5. Middleware
	handlerWithMiddleware := middleware.Chain(
		mux,
		middleware.Recovery,
		middleware.Logger,
	)

	// 6. Setup Server
	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: handlerWithMiddleware,
	}

	// 7. Start Server in Goroutine
	go func() {
		log.Printf("Trae2OpenAI proxy listening on %s (Locale: %s)", addr, cfg.Locale)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// 8. Graceful Shutdown
	quit := make(chan os.Signal, 1)
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL but can't be caught, so don't need to add it
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	// The context is used to inform the server it has 5 seconds to finish
	// the request it is currently handling
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown: ", err)
	}

	log.Println("Server exiting")
}
