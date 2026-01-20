package main

import (
	"log"
	"net/http"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/handler"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

func main() {
	// 1. Load Configuration
	cfg := config.Load()

	// 2. Initialize Services
	traeClient := trae.NewClient(cfg)

	// 3. Initialize Handlers
	h := handler.NewAPIHandler(traeClient)

	// 4. Setup Routes
	mu := http.NewServeMux()
	mu.HandleFunc("/v1/models", h.HandleModels)
	mu.HandleFunc("/v1/chat/completions", h.HandleChatCompletions)

	// 5. Start Server
	log.Println("Trae2OpenAI proxy listening on :8000")
	if err := http.ListenAndServe(":8000", mu); err != nil {
		log.Fatal(err)
	}
}
