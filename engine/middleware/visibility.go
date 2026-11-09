package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/models"
	"github.com/gabehf/koito/internal/utils"
	"github.com/go-chi/chi/v5"
)

func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := GetUserFromContext(r.Context())
		if user == nil {
			utils.WriteError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if user.Role != models.UserRoleAdmin {
			utils.WriteError(w, "admin access required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// VisibleMedia guards direct aliases/interest/credit endpoints as well as
// collection scopes, which could otherwise distinguish a hidden entity.
func VisibleMedia(store db.WebReadStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ids := map[string]string{
				"artist": r.URL.Query().Get("artist_id"),
				"album":  r.URL.Query().Get("album_id"),
				"track":  r.URL.Query().Get("track_id"),
			}
			kind := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/web/v1/"), "/")[0]
			if _, ok := ids[kind]; ok {
				ids[kind] = chi.URLParam(r, "id")
			}
			for kind, value := range ids {
				if value == "" || value == "0" {
					continue
				}
				id, err := strconv.ParseInt(value, 10, 32)
				if err != nil || id <= 0 {
					utils.WriteError(w, "invalid media id", http.StatusBadRequest)
					return
				}
				visible, err := store.IsVisible(r.Context(), kind, int32(id))
				if err != nil {
					utils.WriteError(w, "failed to check media visibility", http.StatusInternalServerError)
					return
				}
				if !visible {
					utils.WriteError(w, "item could not be found", http.StatusNotFound)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
