package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/bobbyo/ccr/db"
)

type contextKey string

const ClientKeyContextKey contextKey = "clientKey"

// Auth returns middleware that validates sk-ccr-xxx tokens against the database.
func Auth(database *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing api key"}`, http.StatusUnauthorized)
				return
			}

			key, err := db.GetKey(database, token)
			if err != nil || key == nil || key.Revoked {
				http.Error(w, `{"error":"invalid or revoked api key"}`, http.StatusUnauthorized)
				return
			}

			// Update last_used asynchronously — don't block the request.
			go func() { _ = db.UpdateLastUsed(database, token) }()

			ctx := context.WithValue(r.Context(), ClientKeyContextKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractToken checks x-api-key header first, then Authorization: Bearer <token>.
func extractToken(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	return ""
}
