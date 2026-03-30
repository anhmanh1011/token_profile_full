package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct {
	store *RedisStore
}

func NewHandler(store *RedisStore) *Handler {
	return &Handler{store: store}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")

	if path == "/stats" && r.Method == http.MethodGet {
		h.handleStatsAll(w, r)
		return
	}

	if strings.HasPrefix(path, "/stats/") && r.Method == http.MethodGet {
		tenantID := strings.TrimPrefix(path, "/stats/")
		h.handleStats(w, r, tenantID)
		return
	}

	if strings.HasSuffix(path, "/exhausted") && r.Method == http.MethodPost {
		parts := strings.Split(path, "/")
		if len(parts) == 4 && parts[1] == "tokens" {
			h.handleExhausted(w, r, parts[2])
			return
		}
	}

	if strings.HasPrefix(path, "/tokens/") && r.Method == http.MethodGet {
		tenantID := strings.TrimPrefix(path, "/tokens/")
		h.handleGetTokens(w, r, tenantID)
		return
	}

	http.NotFound(w, r)
}

func (h *Handler) handleGetTokens(w http.ResponseWriter, r *http.Request, tenantID string) {
	count := 100
	if c := r.URL.Query().Get("count"); c != "" {
		if parsed, err := strconv.Atoi(c); err == nil && parsed > 0 {
			count = parsed
		}
	}

	tokens, err := h.store.PopFreshTokens(r.Context(), tenantID, count)
	if err != nil {
		log.Printf("[ERROR] PopFreshTokens: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, TokenResponse{
		Tokens: tokens,
		Count:  len(tokens),
	})
}

func (h *Handler) handleExhausted(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req ExhaustedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email required"})
		return
	}

	if err := h.store.MarkExhausted(r.Context(), tenantID, req.Email); err != nil {
		log.Printf("[WARN] MarkExhausted: %v", err)
	}

	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request, tenantID string) {
	stats, err := h.store.GetStats(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) handleStatsAll(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.store.GetAllTenants(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	allStats := make([]StatsResponse, 0, len(tenants))
	for _, t := range tenants {
		stats, err := h.store.GetStats(r.Context(), t)
		if err != nil {
			continue
		}
		allStats = append(allStats, stats)
	}

	writeJSON(w, http.StatusOK, allStats)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
