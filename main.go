package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed openapi.yaml
var openapiSpec []byte

const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Books microservice — Swagger UI</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/openapi.yaml", dom_id: "#ui"});</script>
</body></html>`

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(getenv("LOG_LEVEL", "INFO")))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// healthcheck нужен для HEALTHCHECK в образе scratch (там нет curl): `books -healthcheck`.
func healthcheck(port string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func newHandler(api *API) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", methods(map[string]http.HandlerFunc{http.MethodGet: api.health}))
	mux.HandleFunc("/books", methods(map[string]http.HandlerFunc{
		http.MethodGet:  api.listBooks,
		http.MethodPost: api.createBook,
	}))
	mux.HandleFunc("/books/{id}", methods(map[string]http.HandlerFunc{
		http.MethodGet:    api.getBook,
		http.MethodPatch:  api.updateBook,
		http.MethodDelete: api.deleteBook,
	}))
	mux.HandleFunc("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(openapiSpec)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, docsPage)
	})
	mux.HandleFunc("/", notFound)
	return recoverer(logging(mux))
}

func main() {
	port := getenv("PORT", "8000")
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(port))
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := NewStore(ctx, getenv("DATABASE_URL", "postgres://books:books@localhost:5432/books?sslmode=disable"))
	if err != nil {
		slog.Error("cannot start", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           newHandler(&API{store: store}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
