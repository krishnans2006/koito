package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/utils"
)

func GetHiddenArtistsHandler(store db.ArtistVisibilityStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		artists, err := store.GetHiddenArtists(r.Context())
		if err != nil {
			utils.WriteError(w, "failed to get hidden artists", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		utils.WriteJSON(w, http.StatusOK, artists)
	}
}

func SearchVisibilityArtistsHandler(store db.ArtistVisibilityStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(query) > 500 {
			utils.WriteError(w, "search query is too long", http.StatusBadRequest)
			return
		}
		artists, err := store.SearchArtistsForVisibility(r.Context(), query)
		if err != nil {
			utils.WriteError(w, "failed to search artists", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		utils.WriteJSON(w, http.StatusOK, artists)
	}
}

func SetArtistHiddenHandler(store db.ArtistVisibilityStore, hidden bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := utils.ParseIDParam(r, "id")
		if err != nil || id <= 0 {
			utils.WriteError(w, "invalid artist id", http.StatusBadRequest)
			return
		}
		if err := store.SetArtistHidden(r.Context(), id, hidden); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				utils.WriteError(w, "artist could not be found", http.StatusNotFound)
			} else {
				utils.WriteError(w, "failed to update artist visibility", http.StatusInternalServerError)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
