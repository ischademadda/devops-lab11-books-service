package main

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder запоминает код ответа и текст ошибки для лога.
type statusRecorder struct {
	http.ResponseWriter
	status int
	errMsg string
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logging пишет по строке на запрос: метод, путь, статус, время; ошибки — с текстом.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		attrs := []any{
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr,
		}
		switch {
		case rec.status >= 500:
			slog.Error("request failed", append(attrs, "error", rec.errMsg)...)
		case rec.status >= 400:
			slog.Warn("request rejected", append(attrs, "error", rec.errMsg)...)
		default:
			slog.Info("request", attrs...)
		}
	})
}

// recoverer: паника в обработчике не роняет процесс, клиент получает 500 в JSON.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
