package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// scorecard serves GET /api/scorecard: stage 4's eval, the stock's move to
// the next close grouped by the model's score and by the label's level.
func (s *server) scorecard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sc, err := s.store.Scorecard(ctx)
	if err != nil {
		slog.Error("read scorecard", "err", err)
		http.Error(w, "the scorecard is unavailable right now", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(sc)
}
