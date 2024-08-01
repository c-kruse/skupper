package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

func encode(w http.ResponseWriter, status int, v interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("json encoding error: %s", err)
	}
	return nil
}

func requestLogger(log *slog.Logger, r *http.Request) *slog.Logger {
	return log.With(
		slog.String("endpoint", r.URL.Path),
	)
}
