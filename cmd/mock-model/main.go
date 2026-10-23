package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			for _, token := range []string{"Hello", " from", " model-gateway", "!"} {
				payload := fmt.Sprintf(`{"id":"mock-%d","object":"chat.completion.chunk","choices":[{"delta":{"content":%q},"index":0}]}`, time.Now().UnixNano(), token)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
				time.Sleep(30 * time.Millisecond)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      fmt.Sprintf("mock-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "qwen-demo",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": "Hello from model-gateway!",
				},
				"finish_reason": "stop",
			}},
		})
	})

	logger.Info("mock model started", "port", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Error("mock model stopped", "error", err)
	}
}
