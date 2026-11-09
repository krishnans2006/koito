package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gabehf/koito/imagecache"
	"github.com/gabehf/koito/internal/catalog"
	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/db/sqlite"
	"github.com/gabehf/koito/internal/mbz"
	"github.com/gabehf/koito/internal/memkv"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHiddenArtistsWebsite(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.NewInMemory()
	require.NoError(t, err)
	defer store.Close(ctx)
	require.NoError(t, store.Exec(`
 INSERT INTO users VALUES(1,'admin','admin',x'00'),(2,'user','user',x'00');
 INSERT INTO artists(id) VALUES(1),(2);
 INSERT INTO artist_aliases VALUES(1,'Visible artist','Canonical',1),(2,'Secret artist','Canonical',1),(2,'Secret alias','User',0);
 INSERT INTO releases(id) VALUES(1),(2);
 INSERT INTO release_aliases VALUES(1,'Mixed album','Canonical',1),(2,'Secret album','Canonical',1);
 INSERT INTO artist_releases VALUES(1,1,1),(2,1,0),(2,2,1);
 INSERT INTO tracks(id,release_id,duration) VALUES(1,1,120),(2,2,180),(3,1,200);
 INSERT INTO track_aliases VALUES(1,'Visible song','Canonical',1),(2,'Secret song','Canonical',1),(3,'Secret collaboration','Canonical',1);
 INSERT INTO artist_tracks VALUES(1,1,1),(2,2,1),(1,3,1),(2,3,0);
 INSERT INTO listens(track_id,listened_at,user_id) VALUES(1,1704240000,1),(2,1704067200,1),(3,1704153600,1);
 `))
	sessions := map[string]string{}
	for role, id := range map[string]int32{"admin": 1, "user": 2} {
		session, err := store.SaveSession(ctx, id, time.Now().Add(time.Hour), false)
		require.NoError(t, err)
		sessions[role] = session.ID.String()
	}
	router := chi.NewRouter()
	ready := &atomic.Bool{}
	ready.Store(true)
	bindRoutes(router, ready, store, &mbz.MbzMockCaller{})
	request := func(method, path, role string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if role != "" {
			r.AddCookie(&http.Cookie{Name: "koito_session", Value: sessions[role]})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/admin/hidden-artists"}, {"GET", "/admin/artists/search?q=Secret"},
		{"PUT", "/admin/hidden-artists/2"}, {"DELETE", "/admin/hidden-artists/2"},
	} {
		require.Equal(t, 401, request(route.method, "/apis/web/v1"+route.path, "").Code)
		require.Equal(t, 403, request(route.method, "/apis/web/v1"+route.path, "user").Code)
	}
	stats := func() map[string]any {
		response := request("GET", "/apis/web/v1/stats?period=all_time", "")
		require.Equal(t, 200, response.Code, response.Body.String())
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		return result
	}
	require.EqualValues(t, 3, stats()["listen_count"]) // warm cache before hide
	require.Equal(t, 204, request("PUT", "/apis/web/v1/admin/hidden-artists/2", "admin").Code)
	require.EqualValues(t, 1, stats()["listen_count"])
	result := request("GET", "/apis/web/v1/admin/artists/search?q=alias", "admin")
	require.Equal(t, 200, result.Code)
	require.Contains(t, result.Body.String(), `"hidden":true`)
	require.Equal(t, 404, request("PUT", "/apis/web/v1/admin/hidden-artists/99999", "admin").Code)
	require.Equal(t, 400, request("PUT", "/apis/web/v1/admin/hidden-artists/0", "admin").Code)

	for _, role := range []string{"", "admin", "user"} {
		for _, media := range []string{"artist/2", "album/2", "track/2", "track/3"} {
			suffixes := []string{"", "/aliases", "/interest?buckets=5"}
			if media[:6] != "artist" {
				suffixes = append(suffixes, "/artists")
			}
			for _, suffix := range suffixes {
				result := request("GET", "/apis/web/v1/"+media+suffix, role)
				require.Equal(t, 404, result.Code, media+suffix)
			}
		}
		for _, path := range []string{
			"/artist/1", "/album/1", "/track/1", "/album/1/artists", "/album/1/aliases",
			"/top/artists?period=all_time", "/top/albums?period=all_time", "/top/tracks?period=all_time",
			"/top/tracks?period=all_time&album_id=1", "/top/albums?period=all_time&artist_id=1",
			"/listens?period=all_time", "/search?q=Secret", "/summary?year=2024",
			"/listen-activity?year=2024&month=1&artist_id=0&album_id=0&track_id=0", "/artist/1/interest?buckets=5", "/first-activity",
		} {
			result := request("GET", "/apis/web/v1"+path, role)
			require.Equal(t, 200, result.Code, path+" "+result.Body.String())
			require.NotContains(t, result.Body.String(), "Secret", path)
		}
		for _, query := range []string{"artist_id=2", "album_id=2", "track_id=3"} {
			require.Equal(t, 404, request("GET", "/apis/web/v1/listens?"+query, role).Code)
		}
	}
	memkv.Store.Set("1", int32(3))
	t.Cleanup(func() { memkv.Store.Delete("1") })
	result = request("GET", "/apis/web/v1/now-playing", "")
	require.Equal(t, 200, result.Code)
	require.Contains(t, result.Body.String(), `"currently_playing":false`)

	require.Equal(t, 200, request("GET", "/image/00000000-0000-0000-0000-000000000000/64x64.webp", "").Code)
	// Both disk-cache hits and missing files must be denied before image fetching.
	imageID := uuid.New()
	require.NoError(t, store.UpdateArtist(ctx, db.UpdateArtistOpts{ID: 2, Image: imageID, ImageSrc: "User Upload"}))
	imageURL := fmt.Sprintf("/image/%s/128x128.webp", imageID)
	require.Equal(t, 404, request("GET", imageURL, "").Code)
	imagePath := imagecache.BuildImagePath(imageID, imagecache.ImageSizeSmall)
	require.NoError(t, os.MkdirAll(filepath.Dir(imagePath), 0755))
	require.NoError(t, os.WriteFile(imagePath, []byte("cached artwork"), 0644))
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(imagePath)) })
	require.Equal(t, 404, request("GET", imageURL, "").Code)
	require.NoError(t, store.UpdateArtist(ctx, db.UpdateArtistOpts{ID: 1, Image: imageID, ImageSrc: "User Upload"}))
	require.Equal(t, 200, request("GET", imageURL, "").Code)

	require.Equal(t, 204, request("PUT", "/apis/web/v1/admin/hidden-artists/1", "admin").Code)
	emptyStats := stats()
	require.EqualValues(t, 0, emptyStats["listen_count"])
	require.EqualValues(t, 0, emptyStats["avg_daily_plays"])
	require.EqualValues(t, 0, emptyStats["tracks_per_artist"])
	require.Contains(t, request("GET", "/apis/web/v1/first-activity", "").Body.String(), `"time":null`)
	require.NoError(t, catalog.SubmitListen(ctx, store, catalog.SubmitListenOpts{
		Artist: "Secret artist", ArtistNames: []string{"Secret artist"},
		ReleaseTitle: "Secret album", TrackTitle: "Secret song",
		Time: time.Unix(1704412800, 0), UserID: 1, Client: "visibility-test",
		MbzCaller: &mbz.MbzMockCaller{}, SkipCacheImage: true,
	}))
	tracks, err := store.Count("SELECT COUNT(*) FROM tracks")
	require.NoError(t, err)
	require.Equal(t, 3, tracks) // incoming scrobble reuses the hidden track
	require.EqualValues(t, 0, stats()["listen_count"])

	// Hiding keeps the complete stored history; unhide restores it.
	count, err := store.CountListens(ctx, db.Timeframe{Period: db.PeriodAllTime})
	require.NoError(t, err)
	require.EqualValues(t, 4, count)
	require.Equal(t, 204, request("DELETE", "/apis/web/v1/admin/hidden-artists/1", "admin").Code)
	require.Equal(t, 204, request("DELETE", "/apis/web/v1/admin/hidden-artists/2", "admin").Code)
	require.EqualValues(t, 4, stats()["listen_count"])
}
