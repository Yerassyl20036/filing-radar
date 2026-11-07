package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// costs serves GET /api/costs: what the language model has cost, all time,
// today (since midnight UTC) and by purpose.
func (s *server) costs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	c, err := s.store.ModelCosts(ctx)
	if err != nil {
		slog.Error("read model costs", "err", err)
		http.Error(w, "costs are unavailable right now", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(c)
}
