package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gabehf/koito/engine"
	"github.com/gabehf/koito/internal/catalog"
	"github.com/gabehf/koito/internal/cfg"
	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/db/sqlite"
	"github.com/gabehf/koito/internal/importer"
	"github.com/gabehf/koito/internal/logger"
	"github.com/gabehf/koito/internal/mbz"
	"github.com/gabehf/koito/internal/models"
	"github.com/gabehf/koito/internal/utils"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the public importer and its on-disk report/cache format.
// Keep the coverage here with the other service import integration tests.
type youtubeImportMetadata struct {
	VideoID   string            `json:"video_id"`
	Title     string            `json:"title"`
	Artists   []string          `json:"artists"`
	Album     string            `json:"album,omitempty"`
	Duration  int32             `json:"duration_seconds"`
	Sources   map[string]string `json:"sources"`
	RawArtist bool              `json:"uploader_fallback,omitempty"`
}

type youtubeImportReport struct {
	Complete     bool           `json:"complete"`
	Activities   int            `json:"activities"`
	Excluded     int            `json:"excluded_activities"`
	MusicRecords int            `json:"music_records"`
	UniqueVideos int            `json:"unique_videos"`
	Outcomes     map[string]int `json:"outcomes"`
	Records      []struct {
		VideoID       string                 `json:"video_id"`
		Title         string                 `json:"raw_title"`
		Channel       string                 `json:"raw_channel"`
		Timestamp     string                 `json:"raw_timestamp"`
		Time          time.Time              `json:"time"`
		Invalid       string                 `json:"validation_error"`
		Metadata      *youtubeImportMetadata `json:"metadata"`
		MissingFields []string               `json:"missing_fields"`
		AlbumFallback bool                   `json:"album_uses_track_title"`
		Quality       string                 `json:"metadata_quality"`
		TrackID       int32                  `json:"track_id"`
		Outcome       string                 `json:"outcome"`
	} `json:"records"`
}

func youtubeImportContext() context.Context {
	l := zerolog.Nop()
	return logger.NewContext(&l)
}

func youtubeImportStore(t *testing.T) *sqlite.Sqlite {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(cfg.ConfigDir(), "youtube-music")))
	s := newTestDB()
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func youtubeImportFile(t *testing.T, contents string) string {
	t.Helper()
	name := "watch-history-" + uuid.NewString() + ".html"
	dir := filepath.Join(cfg.ConfigDir(), "import")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600))
	t.Cleanup(func() {
		os.Remove(filepath.Join(dir, name))
		os.Remove(filepath.Join(cfg.ConfigDir(), "import_complete", name))
	})
	return name
}

func youtubeImportCache(t *testing.T, meta youtubeImportMetadata) {
	t.Helper()
	dir := filepath.Join(cfg.ConfigDir(), "youtube-music", "metadata-v1")
	require.NoError(t, os.MkdirAll(dir, 0700))
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, meta.VideoID+".json"), data, 0600))
}

func youtubeImportReadReport(t *testing.T, name string) youtubeImportReport {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(cfg.ConfigDir(), "import_reports", name+".*.youtube-music.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var result youtubeImportReport
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

func youtubeImportCard(header, address, title, channel, timestamp string) string {
	artist := ""
	if channel != "" {
		artist = `<a href="https://www.youtube.com/channel/UCtest">` + channel + `</a><br>`
	}
	return `<div class="outer-cell"><div class="header-cell">` + header + `<br></div><div class="content-cell">Watched <a href="` + address + `">` + title + `</a><br>` + artist + timestamp + `<br></div></div>`
}

func youtubeImportHTML(cards ...string) string {
	return "<html><body>" + strings.Join(cards, "") + "</body></html>"
}

type youtubeImportTransport func(*http.Request) (*http.Response, error)

func (f youtubeImportTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Stub public metadata requests at the HTTP boundary without exposing private
// resolver or parser helpers. Other hosts (including the engine) use the real
// transport. These integration tests are deliberately not parallel.
func youtubeImportHTTP(t *testing.T, handler func(path, id string) (int, string)) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = youtubeImportTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "music.youtube.com" && req.URL.Hostname() != "www.youtube.com" {
			return previous.RoundTrip(req)
		}
		status, body := http.StatusOK, `ytcfg.set({"INNERTUBE_CLIENT_VERSION":"test","INNERTUBE_API_KEY":"public-test-key"});`
		if req.URL.Path != "/" {
			id := ""
			if req.Method == http.MethodPost {
				var payload struct {
					VideoID string `json:"videoId"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
				id = payload.VideoID
			} else {
				id = strings.TrimPrefix(req.URL.Query().Get("url"), "https://www.youtube.com/watch?v=")
			}
			status, body = handler(req.URL.Path, id)
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func youtubeImportFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "test_assets", name))
	require.NoError(t, err)
	return string(data)
}

func TestImportYouTubeMusicStartup(t *testing.T) {
	store := newTestDB()
	const filename = "WATCH-HISTORY-youtube-music-engine.HTML"
	const videoID = "enginetrack"
	cacheDir := filepath.Join(cfg.ConfigDir(), "youtube-music", "metadata-v1")
	require.NoError(t, os.MkdirAll(cacheDir, 0700))
	metadata, err := json.Marshal(map[string]any{
		"video_id": videoID, "title": "YouTube Music Engine Test", "artists": []string{"Engine Artist"},
		"album": "Engine Album", "duration_seconds": 204,
		"sources": map[string]string{"title": "next", "artists": "next_artist", "album": "next", "duration": "next"},
	})
	require.NoError(t, err)
	cachePath := filepath.Join(cacheDir, videoID+".json")
	require.NoError(t, os.WriteFile(cachePath, metadata, 0600))
	t.Cleanup(func() { os.Remove(cachePath) })
	input := `<html><body><div class="outer-cell"><div class="header-cell">YouTube Music<br></div><div class="content-cell">Watched <a href="https://music.youtube.com/watch?v=enginetrack">Raw title</a><br><a href="https://www.youtube.com/channel/UCtest">Raw channel</a><br>Jul 4, 2026, 1:02:03 PM CST<br></div></div><div class="outer-cell"><div class="header-cell">YouTube<br></div><div class="content-cell">Watched <a href="https://www.youtube.com/watch?v=enginetrack">YouTube Music Engine Test</a><br>Jul 4, 2026, 1:02:03 PM CST<br></div></div></body></html>`
	importPath := filepath.Join(cfg.ConfigDir(), "import", filename)
	require.NoError(t, os.WriteFile(importPath, []byte(input), 0600))
	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})
	ctx := context.Background()
	artist, err := store.GetArtist(ctx, db.GetArtistOpts{Name: "Engine Artist"})
	require.NoError(t, err)
	album, err := store.GetAlbum(ctx, db.GetAlbumOpts{Title: "Engine Album", ArtistID: artist.ID})
	require.NoError(t, err)
	track, err := store.GetTrack(ctx, db.GetTrackOpts{Title: "YouTube Music Engine Test", ReleaseID: album.ID, ArtistIDs: []int32{artist.ID}})
	require.NoError(t, err)
	assert.EqualValues(t, 204, track.Duration)
	assert.EqualValues(t, 1, track.ListenCount)
	require.FileExists(t, filepath.Join(cfg.ConfigDir(), "import_complete", filename))
	files, err := filepath.Glob(filepath.Join(cfg.ConfigDir(), "import_reports", filename+".*.youtube-music.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	reportBytes, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var report struct {
		Music    int            `json:"music_records"`
		Excluded int            `json:"excluded_activities"`
		Outcomes map[string]int `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal(reportBytes, &report))
	assert.Equal(t, 1, report.Music)
	assert.Equal(t, 1, report.Excluded)
	assert.Equal(t, 1, report.Outcomes["submitted"])
}

func TestImportYouTubeMusicMetadataAndCounts(t *testing.T) {
	s := youtubeImportStore(t)
	name := youtubeImportFile(t, youtubeImportFixture(t, "youtube-music-history.html"))
	fixture := youtubeImportFixture(t, "youtube-music-next.json")
	calls := make(map[string]int)
	youtubeImportHTTP(t, func(path, id string) (int, string) {
		calls[path+"/"+id]++
		switch path {
		case "/youtubei/v1/next":
			return http.StatusOK, fixture
		case "/youtubei/v1/player":
			return http.StatusOK, `{"playabilityStatus":{"status":"ERROR"}}`
		case "/oembed":
			return http.StatusNotFound, ""
		default:
			t.Fatalf("unexpected metadata path %s", path)
			return 0, ""
		}
	})
	ctx := youtubeImportContext()
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r := youtubeImportReadReport(t, name)
	assert.True(t, r.Complete)
	assert.Equal(t, 6, r.Activities)
	assert.Equal(t, 2, r.Excluded)
	assert.Equal(t, 4, r.MusicRecords)
	assert.Equal(t, 3, r.UniqueVideos)
	assert.Equal(t, 3, r.Outcomes["submitted"])
	assert.Equal(t, 1, r.Outcomes["unresolved"])
	require.Len(t, r.Records, 4)
	a := r.Records[0]
	assert.Equal(t, "Song & More (Live)", a.Title)
	assert.Equal(t, "Song & More (Live) (feat. Guest)", a.Metadata.Title)
	assert.Equal(t, []string{"Earth, Wind & Fire", "Guest"}, a.Metadata.Artists)
	assert.Equal(t, "Live Album", a.Metadata.Album)
	assert.EqualValues(t, 204, a.Metadata.Duration)
	assert.Equal(t, "resolved", a.Quality)
	assert.Contains(t, a.Timestamp, "\u202f")
	assert.Equal(t, time.Date(2026, 7, 4, 19, 2, 3, 0, time.UTC), a.Time)
	assert.Equal(t, a.Time, r.Records[2].Time, "different songs at the same second are retained")
	assert.NotEqual(t, a.TrackID, r.Records[2].TrackID)
	assert.Equal(t, 1, calls["/youtubei/v1/next/aaaaaaaaaaa"], "repeat listens share metadata")
	assert.Equal(t, 0, calls["/youtubei/v1/player/aaaaaaaaaaa"])
	b := r.Records[2]
	assert.True(t, b.AlbumFallback)
	assert.Empty(t, b.Metadata.Album)
	assert.Equal(t, "partial", b.Quality)
	var album string
	require.NoError(t, s.QueryRow(`SELECT r.title FROM tracks t JOIN releases_with_title r ON t.release_id=r.id WHERE t.id=?`, b.TrackID).Scan(&album))
	assert.Equal(t, "Other Song (Acoustic)", album)
	count, err := s.Count(`SELECT COUNT(*) FROM listens WHERE client='youtube-music'`)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	require.NoError(t, os.Rename(filepath.Join(cfg.ConfigDir(), "import_complete", name), filepath.Join(cfg.ConfigDir(), "import", name)))
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r = youtubeImportReadReport(t, name)
	assert.Zero(t, r.Outcomes["submitted"])
	assert.Equal(t, 3, r.Outcomes["already_present"])
	assert.Equal(t, 1, calls["/youtubei/v1/next/aaaaaaaaaaa"])
	assert.Equal(t, 2, calls["/youtubei/v1/next/ddddddddddd"], "unresolved IDs are retried")
}

func TestImportYouTubeMusicPlayerAndRawFallbacks(t *testing.T) {
	s := youtubeImportStore(t)
	fixture := youtubeImportFixture(t, "youtube-music-next.json")
	source := youtubeImportHTML(
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=ccccccccccc", "Raw Edit", "Raw Uploader", "Jul 4, 2026, 1:00:00 PM CST"),
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=eeeeeeeeeee", "Raw Song", "Raw Channel", "Jul 4, 2026, 1:02:03 PM CST"),
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=fffffffffff", "Raw History Title (Acoustic) &amp;amp; &NoBreak;&NotEqualTilde; &#128;", "History Artist - Topic", "Jul 4, 2026, 1:05:00 PM CST"),
	)
	// HTML raw-text elements and character references require HTML semantics:
	// fake cards in CSS/JavaScript must stay text, and entities decode once.
	source = strings.Replace(source, "<body>", `<head><style>.example:before {content: '<div class="outer-cell">';}</style><script>const fake = '<div class="outer-cell">'; if (1 < 2) {}</script></head><body>`, 1)
	name := youtubeImportFile(t, source)
	youtubeImportHTTP(t, func(path, id string) (int, string) {
		switch path {
		case "/youtubei/v1/next":
			return http.StatusOK, fixture
		case "/youtubei/v1/player":
			if id == "eeeeeeeeeee" {
				return http.StatusOK, `{"videoDetails":{"videoId":"eeeeeeeeeee","title":"Recovered Song (Acoustic)","author":"Recovered Artist - Topic","lengthSeconds":"245","musicVideoType":"MUSIC_VIDEO_TYPE_ATV"},"playabilityStatus":{"status":"UNPLAYABLE"}}`
			}
			return http.StatusOK, `{"playabilityStatus":{"status":"ERROR"}}`
		case "/oembed":
			return http.StatusNotFound, ""
		default:
			t.Fatalf("unexpected path %s", path)
			return 0, ""
		}
	})
	require.NoError(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
	r := youtubeImportReadReport(t, name)
	assert.Equal(t, 3, r.Activities, "raw-text content must not create extra cards")
	assert.Equal(t, 3, r.Outcomes["submitted"])
	assert.Equal(t, "Other Song (User Edit)", r.Records[0].Metadata.Title)
	assert.Equal(t, []string{"An Uploader"}, r.Records[0].Metadata.Artists)
	assert.True(t, r.Records[0].Metadata.RawArtist)
	assert.Equal(t, "raw", r.Records[0].Quality)
	assert.Equal(t, "Recovered Song (Acoustic)", r.Records[1].Metadata.Title)
	assert.Equal(t, []string{"Recovered Artist"}, r.Records[1].Metadata.Artists)
	assert.EqualValues(t, 245, r.Records[1].Metadata.Duration)
	assert.Equal(t, "Raw History Title (Acoustic) &amp; \u2060\u2242\u0338 \u20ac", r.Records[2].Metadata.Title)
	assert.Equal(t, []string{"History Artist"}, r.Records[2].Metadata.Artists)
	assert.Equal(t, "history", r.Records[2].Metadata.Sources["title"])
	assert.Equal(t, []string{"album", "duration"}, r.Records[2].MissingFields)
	assert.Zero(t, r.Records[2].Metadata.Duration)
}

func TestImportYouTubeMusicValidationAndTimestamps(t *testing.T) {
	s := youtubeImportStore(t)
	youtubeImportCache(t, youtubeImportMetadata{VideoID: "aaaaaaaaaaa", Title: "Song", Artists: []string{"Artist"}, Sources: map[string]string{"title": "next", "artists": "next_artist"}})
	youtubeImportHTTP(t, func(path, id string) (int, string) {
		t.Fatalf("unexpected metadata lookup: %s/%s", path, id)
		return 0, ""
	})
	var cards []string
	var times []time.Time
	for _, pair := range [][2]string{
		{"Jul 4, 2026, 1:02:03\u202fPM CST", "2026-07-04T19:02:03Z"},
		{"Jul 4, 2026, 1:02:03 PM CDT", "2026-07-04T18:02:03Z"},
		{"Jan 4, 2026, 12:00:00 AM PST", "2026-01-04T08:00:00Z"},
		{"Jul 4, 2026, 1:02:03 PM UTC", "2026-07-04T13:02:03Z"},
		{"Jul 4, 2026, 1:02:03 PM +05:30", "2026-07-04T07:32:03Z"},
		{"Jul 4, 2026, 1:02:03 PM -0600", "2026-07-04T19:02:03Z"},
		{"2026-07-04T13:02:03-06:00", "2026-07-04T19:02:03Z"},
	} {
		cards = append(cards, youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", pair[0]))
		ts, err := time.Parse(time.RFC3339, pair[1])
		require.NoError(t, err)
		times = append(times, ts)
	}
	date := "Jul 4, 2026, 1:02:03 PM CST"
	cards = append(cards,
		youtubeImportCard("YouTube Music", "https://music.youtube.com.evil.test/watch?v=aaaaaaaaaaa", "Song", "Artist", date),
		youtubeImportCard("YouTube", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", date),
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=short", "Song", "Artist", date),
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa&amp;v=bbbbbbbbbbb", "Song", "Artist", date),
		youtubeImportCard("YouTube Music", "https://user@music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", date),
		youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", "Jul 4, 2026, 1:02:03 PM XYZ"),
		strings.Replace(youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", date), "Watched ", "Liked ", 1),
		youtubeImportCard("YouTube", "https://www.youtube.com/watch?v=aaaaaaaaaaa", "YouTube Music compared", "Artist", date),
		youtubeImportCard("YouTube TV", "https://www.youtube.com/watch?v=aaaaaaaaaaa", "TV", "Artist", date),
	)
	name := youtubeImportFile(t, youtubeImportHTML(cards...))
	require.NoError(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
	r := youtubeImportReadReport(t, name)
	assert.Equal(t, 7, r.Outcomes["invalid"])
	assert.Equal(t, 2, r.Excluded)
	for i, ts := range times {
		assert.Equal(t, ts, r.Records[i].Time)
	}
	for _, record := range r.Records[len(times):] {
		assert.NotEmpty(t, record.Invalid)
		assert.NotEmpty(t, record.Timestamp)
	}

	name = youtubeImportFile(t, strings.TrimSuffix(youtubeImportHTML(cards[0]), "</body></html>"))
	require.Error(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
	assert.False(t, youtubeImportReadReport(t, name).Complete)
	require.FileExists(t, filepath.Join(cfg.ConfigDir(), "import", name))
	ctx, cancel := context.WithCancel(youtubeImportContext())
	cancel()
	name = youtubeImportFile(t, youtubeImportHTML(cards[0]))
	require.ErrorIs(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name), context.Canceled)
}

func TestImportYouTubeMusicRetryAndSchemaFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := youtubeImportStore(t)
			name := youtubeImportFile(t, youtubeImportHTML(
				youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", "Jul 4, 2026, 1:00:00 PM CST"),
				youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=bbbbbbbbbbb", "Other", "Other Artist", "Jul 4, 2026, 1:02:03 PM CST"),
			))
			fixture := youtubeImportFixture(t, "youtube-music-next.json")
			calls, failing := 0, true
			youtubeImportHTTP(t, func(path, id string) (int, string) {
				require.Equal(t, "/youtubei/v1/next", path)
				if id == "aaaaaaaaaaa" {
					calls++
					if calls < 3 {
						return status, ""
					}
				}
				if id == "bbbbbbbbbbb" && failing {
					return http.StatusForbidden, ""
				}
				return http.StatusOK, fixture
			})
			require.Error(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
			r := youtubeImportReadReport(t, name)
			assert.False(t, r.Complete)
			assert.Equal(t, 1, r.Outcomes["submitted"])
			assert.Equal(t, 1, r.Outcomes["pending"])
			assert.Equal(t, 3, calls)
			require.FileExists(t, filepath.Join(cfg.ConfigDir(), "import", name))
			failing = false
			require.NoError(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
			r = youtubeImportReadReport(t, name)
			assert.True(t, r.Complete)
			assert.Equal(t, 1, r.Outcomes["submitted"])
			assert.Equal(t, 1, r.Outcomes["already_present"])
			assert.Equal(t, 3, calls)
		})
	}
	for _, schema := range []string{"malformed", "changed renderer"} {
		t.Run(schema, func(t *testing.T) {
			s := youtubeImportStore(t)
			name := youtubeImportFile(t, youtubeImportHTML(youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", "Jul 4, 2026, 1:00:00 PM CST")))
			body := "not JSON"
			if schema == "changed renderer" {
				body = strings.ReplaceAll(youtubeImportFixture(t, "youtube-music-next.json"), "playlistPanelVideoRenderer", "changedVideoRenderer")
			}
			youtubeImportHTTP(t, func(path, id string) (int, string) { return http.StatusOK, body })
			require.Error(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
			r := youtubeImportReadReport(t, name)
			assert.False(t, r.Complete)
			assert.Equal(t, 1, r.Outcomes["pending"])
			assert.Zero(t, r.Outcomes["submitted"])
		})
	}
}

func TestImportYouTubeMusicLaterHistoryRecoversIdentity(t *testing.T) {
	s := youtubeImportStore(t)
	address := "https://music.youtube.com/watch?v=aaaaaaaaaaa"
	name := youtubeImportFile(t, youtubeImportHTML(
		youtubeImportCard("YouTube Music", address, address, "", "Jul 4, 2026, 1:00:00 PM CST"),
		youtubeImportCard("YouTube Music", address, "Recovered Song", "Recovered Artist - Topic", "Jul 4, 2026, 1:02:03 PM CST"),
	))
	fixture := strings.ReplaceAll(youtubeImportFixture(t, "youtube-music-next.json"), "aaaaaaaaaaa", "zzzzzzzzzzz")
	lookups := 0
	youtubeImportHTTP(t, func(path, id string) (int, string) {
		switch path {
		case "/youtubei/v1/next":
			lookups++
			return http.StatusOK, fixture
		case "/youtubei/v1/player":
			return http.StatusOK, `{"playabilityStatus":{"status":"ERROR"}}`
		case "/oembed":
			return http.StatusNotFound, ""
		default:
			t.Fatalf("unexpected path %s", path)
			return 0, ""
		}
	})
	ctx := youtubeImportContext()
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r := youtubeImportReadReport(t, name)
	assert.Equal(t, 1, r.Outcomes["unresolved"])
	assert.Equal(t, 1, r.Outcomes["submitted"])
	assert.Empty(t, r.Records[0].Metadata.Sources, "later fallback must not rewrite earlier diagnostics")
	assert.Equal(t, "Recovered Song", r.Records[1].Metadata.Title)
	require.NoError(t, os.Rename(filepath.Join(cfg.ConfigDir(), "import_complete", name), filepath.Join(cfg.ConfigDir(), "import", name)))
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r = youtubeImportReadReport(t, name)
	assert.Equal(t, 1, r.Outcomes["submitted"])
	assert.Equal(t, 1, r.Outcomes["already_present"])
	assert.Zero(t, r.Outcomes["unresolved"])
	assert.Equal(t, 1, lookups)
}

func TestImportYouTubeMusicFilesystemFailures(t *testing.T) {
	s := youtubeImportStore(t)
	for _, folder := range []string{"import_reports", "import_complete"} {
		t.Run(folder, func(t *testing.T) {
			name := youtubeImportFile(t, youtubeImportHTML())
			dir := filepath.Join(cfg.ConfigDir(), folder)
			require.NoError(t, os.MkdirAll(dir, 0700))
			backup := dir + "-backup"
			require.NoError(t, os.Rename(dir, backup))
			t.Cleanup(func() { os.Remove(dir); os.Rename(backup, dir) })
			require.NoError(t, os.WriteFile(dir, []byte("blocked"), 0600))
			require.Error(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
			require.FileExists(t, filepath.Join(cfg.ConfigDir(), "import", name))
			if folder == "import_complete" {
				assert.False(t, youtubeImportReadReport(t, name).Complete)
			}
		})
	}
}

// Simulate a second writer inserting the same key after the importer's lookup.
// The original SaveListen still deduplicates it. Reports describe submissions,
// not the ownership of the insert, which the existing DB API cannot expose.
type youtubeImportConcurrentWriter struct {
	db.DB
	writes int
}

func (s *youtubeImportConcurrentWriter) SaveListen(ctx context.Context, opts db.SaveListenOpts) error {
	s.writes++
	if err := s.DB.SaveListen(ctx, opts); err != nil {
		return err
	}
	return s.DB.SaveListen(ctx, opts)
}

func TestImportYouTubeMusicExistingStoreAPIAndConcurrentDuplicate(t *testing.T) {
	database := youtubeImportStore(t)
	s := &youtubeImportConcurrentWriter{DB: database}
	youtubeImportCache(t, youtubeImportMetadata{VideoID: "aaaaaaaaaaa", Title: "Song", Artists: []string{"Artist"}, Sources: map[string]string{"title": "fixture", "artists": "fixture"}})
	name := youtubeImportFile(t, youtubeImportHTML(youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", "Jul 4, 2026, 1:00:00 PM CST")))
	ctx := youtubeImportContext()
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r := youtubeImportReadReport(t, name)
	assert.Equal(t, 1, r.Outcomes["submitted"])
	assert.NotContains(t, r.Outcomes, "inserted")
	count, err := database.Count(`SELECT COUNT(*) FROM listens`)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.NoError(t, os.Rename(filepath.Join(cfg.ConfigDir(), "import_complete", name), filepath.Join(cfg.ConfigDir(), "import", name)))
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, s, &mbz.MbzErrorCaller{}, name))
	r = youtubeImportReadReport(t, name)
	assert.Equal(t, 1, r.Outcomes["already_present"])
	assert.Zero(t, r.Outcomes["submitted"])
	assert.Equal(t, 2, s.writes)
	count, err = database.Count(`SELECT COUNT(*) FROM listens`)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// Stop after parsing but before any database mutation, so the large personal
// sample can still be audited through the public entry point in ordinary tests.
type youtubeImportAuditStore struct {
	db.DB
	err error
}

func (s youtubeImportAuditStore) GetArtist(context.Context, db.GetArtistOpts) (*models.Artist, error) {
	return nil, s.err
}

func TestImportYouTubeMusicSampleAudit(t *testing.T) {
	filename := os.Getenv("KOITO_YOUTUBE_MUSIC_HISTORY")
	if filename == "" {
		filename = filepath.Join("..", "watch-history.html")
	}
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("personal sample is intentionally not committed")
	}
	require.NoError(t, err)
	_ = youtubeImportStore(t)
	name := youtubeImportFile(t, string(data))
	first := regexp.MustCompile(`href="https://music.youtube.com/watch\?v=([A-Za-z0-9_-]{11})"`).FindSubmatch(data)
	require.Len(t, first, 2)
	youtubeImportCache(t, youtubeImportMetadata{VideoID: string(first[1]), Title: "Audit", Artists: []string{"Audit Artist"}, Sources: map[string]string{"title": "fixture", "artists": "fixture"}})
	stop := errors.New("stop before catalog mutation")
	err = importer.ImportYouTubeMusicFile(youtubeImportContext(), youtubeImportAuditStore{err: stop}, &mbz.MbzErrorCaller{}, name)
	require.ErrorIs(t, err, stop)
	r := youtubeImportReadReport(t, name)
	assert.Equal(t, 48570, r.Activities)
	assert.Equal(t, 18952, r.MusicRecords)
	assert.Equal(t, 2710, r.UniqueVideos)
	assert.Equal(t, 29618, r.Excluded)
	assert.Equal(t, 18954, strings.Count(string(data), "music.youtube.com"))
	missing, duplicates := 0, 0
	seen := make(map[string]bool)
	for _, record := range r.Records {
		require.Empty(t, record.Invalid)
		if record.Channel == "" || strings.HasPrefix(record.Title, "https://") {
			missing++
		}
		key := fmt.Sprintf("%s/%d", record.VideoID, record.Time.Unix())
		if seen[key] {
			duplicates++
		}
		seen[key] = true
	}
	assert.Equal(t, 2, missing)
	assert.Zero(t, duplicates)
	if os.Getenv("KOITO_TEST_YOUTUBE_MUSIC_FULL_IMPORT") != "1" {
		return
	}

	s := youtubeImportStore(t)
	// Seed deterministic unavailable-provider fallbacks from the known export's
	// title/channel fields. Only URL-only IDs need the HTTP fallback fixtures.
	watch := regexp.MustCompile(`<a href="https://music.youtube.com/watch\?v=([A-Za-z0-9_-]{11})">([^<]*)</a>`)
	channel := regexp.MustCompile(`<a href="https://www.youtube.com/channel/[^"]+">([^<]*)</a>`)
	for _, card := range strings.Split(string(data), `<div class="outer-cell `) {
		w, a := watch.FindStringSubmatch(card), channel.FindStringSubmatch(card)
		if len(w) != 3 || len(a) != 2 || strings.HasPrefix(w[2], "https://") {
			continue
		}
		youtubeImportCache(t, youtubeImportMetadata{VideoID: w[1], Title: html.UnescapeString(w[2]), Artists: []string{strings.TrimSuffix(html.UnescapeString(a[1]), " - Topic")}, RawArtist: true, Sources: map[string]string{"title": "history", "artists": "history_channel"}})
	}
	fixture := youtubeImportFixture(t, "youtube-music-next.json")
	youtubeImportHTTP(t, func(path, id string) (int, string) {
		if path == "/youtubei/v1/next" {
			return http.StatusOK, fixture
		}
		if path == "/youtubei/v1/player" {
			return http.StatusOK, `{"playabilityStatus":{"status":"ERROR"}}`
		}
		return http.StatusNotFound, ""
	})
	require.NoError(t, importer.ImportYouTubeMusicFile(youtubeImportContext(), s, &mbz.MbzErrorCaller{}, name))
	r = youtubeImportReadReport(t, name)
	count, err := s.Count(`SELECT COUNT(*) FROM listens`)
	require.NoError(t, err)
	assert.Equal(t, 18950, count)
	assert.Equal(t, 18950, r.Outcomes["submitted"])
	assert.Equal(t, 2, r.Outcomes["unresolved"])
}

func TestImportMaloja(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "maloja_import_test.json")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "maloja_import_test.json")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	// maloja test import is 38 Magnify Tokyo streams
	a, err := store.GetArtist(context.Background(), db.GetArtistOpts{Name: "Magnify Tokyo"})
	require.NoError(t, err)
	t.Log(a)
	assert.Equal(t, "Magnify Tokyo", a.Name)
	assert.EqualValues(t, 38, a.ListenCount)
}

func TestImportSpotify(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "Streaming_History_Audio_spotify_import_test.json")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "Streaming_History_Audio_spotify_import_test.json")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	a, err := store.GetArtist(context.Background(), db.GetArtistOpts{Name: "The Story So Far"})
	require.NoError(t, err)
	r, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{ArtistID: a.ID, Title: "The Story So Far / Stick To Your Guns Split"})
	require.NoError(t, err)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{Title: "Clairvoyant", ReleaseID: r.ID, ArtistIDs: []int32{a.ID}})
	require.NoError(t, err)
	t.Log(track)
	assert.Equal(t, "Clairvoyant", track.Title)
	// spotify includes duration data, but we only import when reason_end = trackdone
	// this is the only track with valid duration data
	assert.EqualValues(t, 181, track.Duration)
}

func TestImportLastFM(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "recenttracks-shoko2-1749776100.json")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "recenttracks-shoko2-1749776100.json")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	mbzcMock := &mbz.MbzMockCaller{
		Artists: map[uuid.UUID]*mbz.MusicBrainzArtist{
			uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a"): {
				Name: "OurR",
				Aliases: []mbz.MusicBrainzArtistAlias{
					{Name: "OurR", Primary: true},
				},
			},
		},
	}

	engine.RunImporter(logger.Get(), store, mbzcMock)

	album, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{MusicBrainzID: uuid.MustParse("e9e78802-0bf8-4ca3-9655-1d943d2d2fa0")})
	require.NoError(t, err)
	assert.Equal(t, "ZOO!!", album.Title)
	artist, err := store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a")})
	require.NoError(t, err)
	assert.Equal(t, "OurR", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{Name: "Necry Talkie"})
	require.NoError(t, err)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{Title: "放課後の記憶", ReleaseID: album.ID, ArtistIDs: []int32{artist.ID}})
	require.NoError(t, err)
	t.Log(track)
	listens, err := store.GetListensPaginated(context.Background(), db.GetItemsOpts{TrackID: int(track.ID), Timeframe: db.Timeframe{Period: db.PeriodAllTime}})
	require.NoError(t, err)
	require.Len(t, listens.Items, 1)
	assert.WithinDuration(t, time.Unix(1749774900, 0), listens.Items[0].Time, 1*time.Second)
}

func TestImportLastFM_MbzDisabled(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "recenttracks-shoko2-1749776100.json")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "recenttracks-shoko2-1749776100.json")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	album, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{MusicBrainzID: uuid.MustParse("e9e78802-0bf8-4ca3-9655-1d943d2d2fa0")})
	require.NoError(t, err)
	assert.Equal(t, "ZOO!!", album.Title)
	artist, err := store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a")})
	require.NoError(t, err)
	assert.Equal(t, "OurR", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{Name: "Necry Talkie"})
	require.NoError(t, err)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{Title: "放課後の記憶", ReleaseID: album.ID, ArtistIDs: []int32{artist.ID}})
	require.NoError(t, err)
	t.Log(track)
	listens, err := store.GetListensPaginated(context.Background(), db.GetItemsOpts{TrackID: int(track.ID), Timeframe: db.Timeframe{Period: db.PeriodAllTime}})
	require.NoError(t, err)
	require.Len(t, listens.Items, 1)
	assert.WithinDuration(t, time.Unix(1749774900, 0), listens.Items[0].Time, 1*time.Second)
}

func TestImportListenBrainz(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "listenbrainz_shoko1_1749780844.zip")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "listenbrainz_shoko1_1749780844.zip")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	mbzcMock := &mbz.MbzMockCaller{
		Artists: map[uuid.UUID]*mbz.MusicBrainzArtist{
			uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a"): {
				Name: "OurR",
				Aliases: []mbz.MusicBrainzArtistAlias{
					{Name: "OurR", Primary: true},
				},
			},
			uuid.MustParse("09887aa7-226e-4ecc-9a0c-02d2ae5777e1"): {
				Name: "Carly Rae Jepsen",
				Aliases: []mbz.MusicBrainzArtistAlias{
					{Name: "Carly Rae Jepsen", Primary: true},
				},
			},
			uuid.MustParse("78e46ae5-9bfd-433b-be3f-19e993d67ecc"): {
				Name: "Rufus Wainwright",
				Aliases: []mbz.MusicBrainzArtistAlias{
					{Name: "OurR", Primary: true},
				},
			},
		},
	}

	engine.RunImporter(logger.Get(), store, mbzcMock)

	album, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{MusicBrainzID: uuid.MustParse("ce330d67-9c46-4a3b-9d62-08406370f234")})
	require.NoError(t, err)
	assert.Equal(t, "酸欠少女", album.Title)
	artist, err := store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a")})
	require.NoError(t, err)
	assert.Equal(t, "OurR", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("09887aa7-226e-4ecc-9a0c-02d2ae5777e1")})
	require.NoError(t, err)
	assert.Equal(t, "Carly Rae Jepsen", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("78e46ae5-9bfd-433b-be3f-19e993d67ecc")})
	require.NoError(t, err)
	assert.Equal(t, "Rufus Wainwright", artist.Name)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{MusicBrainzID: uuid.MustParse("08e8f55b-f1a4-46b8-b2d1-fab4c592165c")})
	require.NoError(t, err)
	assert.Equal(t, "Desert", track.Title)
	listens, err := store.GetListensPaginated(context.Background(), db.GetItemsOpts{TrackID: int(track.ID), Timeframe: db.Timeframe{Period: db.PeriodAllTime}})
	require.NoError(t, err)
	assert.Len(t, listens.Items, 1)
	assert.WithinDuration(t, time.Unix(1749780612, 0), listens.Items[0].Time, 1*time.Second)
}

func TestImportListenBrainz_MbzDisabled(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "listenbrainz_shoko1_1749780844.zip")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "listenbrainz_shoko1_1749780844.zip")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	album, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{MusicBrainzID: uuid.MustParse("ce330d67-9c46-4a3b-9d62-08406370f234")})
	require.NoError(t, err)
	assert.Equal(t, "酸欠少女", album.Title)
	artist, err := store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("4b00640f-3be6-43f8-9b34-ff81bd89320a")})
	require.NoError(t, err)
	assert.Equal(t, "OurR", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("09887aa7-226e-4ecc-9a0c-02d2ae5777e1")})
	require.NoError(t, err)
	assert.Equal(t, "Carly Rae Jepsen", artist.Name)
	artist, err = store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("78e46ae5-9bfd-433b-be3f-19e993d67ecc")})
	require.NoError(t, err)
	assert.Equal(t, "Rufus Wainwright", artist.Name)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{MusicBrainzID: uuid.MustParse("08e8f55b-f1a4-46b8-b2d1-fab4c592165c")})
	require.NoError(t, err)
	assert.Equal(t, "Desert", track.Title)
	listens, err := store.GetListensPaginated(context.Background(), db.GetItemsOpts{TrackID: int(track.ID), Timeframe: db.Timeframe{Period: db.PeriodAllTime}})
	require.NoError(t, err)
	assert.Len(t, listens.Items, 1)
	assert.WithinDuration(t, time.Unix(1749780612, 0), listens.Items[0].Time, 1*time.Second)
}

func TestImportListenBrainz_MBIDMapping(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "listenbrainz_shoko1_123456789.zip")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "listenbrainz_shoko1_123456789.zip")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	album, err := store.GetAlbum(context.Background(), db.GetAlbumOpts{MusicBrainzID: uuid.MustParse("177ebc28-0115-3897-8eb3-ebf74ce23790")})
	require.NoError(t, err)
	assert.Equal(t, "Zombie", album.Title)
	artist, err := store.GetArtist(context.Background(), db.GetArtistOpts{MusicBrainzID: uuid.MustParse("c98d40fd-f6cf-4b26-883e-eaa515ee2851")})
	require.NoError(t, err)
	assert.Equal(t, "The Cranberries", artist.Name)
	track, err := store.GetTrack(context.Background(), db.GetTrackOpts{MusicBrainzID: uuid.MustParse("3bbeb4e3-ab6d-460d-bfc5-de49e4251061")})
	require.NoError(t, err)
	assert.Equal(t, "Zombie", track.Title)
}

func TestImportKoito(t *testing.T) {
	store := newTestDB()

	src := path.Join("..", "test_assets", "koito_export_test.json")
	destDir := filepath.Join(cfg.ConfigDir(), "import")
	dest := filepath.Join(destDir, "koito_export_test.json")

	ctx := context.Background()

	// 4 every wave to ever rise, 3 i can't feel you, 5 giri giri, 1 nijinoiroyo
	giriReleaseMBID := uuid.MustParse("ac1f8da0-21d7-426e-83b0-befff06f0871")
	suzukiMBID := uuid.MustParse("30f851bb-dba3-4e9b-811c-5f27f595c86a")
	nijinoTrackMBID := uuid.MustParse("a4f26836-3894-46c1-acac-227808308687")
	lp3MBID := uuid.MustParse("d0ec30bd-7cdc-417c-979d-5a0631b8a161")

	input, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, input, os.ModePerm))

	engine.RunImporter(logger.Get(), store, &mbz.MbzErrorCaller{})

	// ensure all artists are saved
	_, err = store.GetArtist(ctx, db.GetArtistOpts{Name: "American Football"})
	assert.NoError(t, err)
	_, err = store.GetArtist(ctx, db.GetArtistOpts{Name: "Rachel Goswell"})
	assert.NoError(t, err)
	_, err = store.GetArtist(ctx, db.GetArtistOpts{Name: "Elizabeth Powell"})
	assert.NoError(t, err)

	// ensure client is saved
	ctn, err := store.Count("SELECT COUNT(*) FROM listens WHERE client IS NOT '';")
	require.NoError(t, err)
	assert.Greater(t, ctn, 0)

	// ensure artist aliases are saved
	artist, err := store.GetArtist(ctx, db.GetArtistOpts{MusicBrainzID: suzukiMBID})
	require.NoError(t, err)
	assert.Equal(t, "鈴木雅之", artist.Name)
	assert.Contains(t, artist.Aliases, "Masayuki Suzuki")
	_, err = store.GetArtist(ctx, db.GetArtistOpts{Name: "すぅ"})
	require.NoError(t, err)

	// ensure albums are saved
	album, err := store.GetAlbum(ctx, db.GetAlbumOpts{MusicBrainzID: giriReleaseMBID})
	require.NoError(t, err)
	assert.Equal(t, "GIRI GIRI", album.Title)
	// ensure album aliases are saved
	artist, err = store.GetArtist(ctx, db.GetArtistOpts{Name: "NELKE"})
	require.NoError(t, err)
	album, err = store.GetAlbum(ctx, db.GetAlbumOpts{Title: "虹の色よ鮮やかであれ (NELKE ver.)", ArtistID: artist.ID})
	require.NoError(t, err)
	aliases, err := store.GetAllAlbumAliases(ctx, album.ID)
	require.NoError(t, err)
	assert.Contains(t, utils.FlattenAliases(aliases), "Nijinoiroyo Azayakadeare (NELKE ver.)")
	// ensure album associations are saved
	album, err = store.GetAlbum(ctx, db.GetAlbumOpts{MusicBrainzID: lp3MBID})
	require.NoError(t, err)
	assert.Contains(t, utils.FlattenSimpleArtistNames(album.Artists), "Elizabeth Powell")
	assert.Contains(t, utils.FlattenSimpleArtistNames(album.Artists), "Rachel Goswell")
	assert.Contains(t, utils.FlattenSimpleArtistNames(album.Artists), "American Football")

	// ensure all tracks are saved
	track, err := store.GetTrack(ctx, db.GetTrackOpts{MusicBrainzID: nijinoTrackMBID})
	require.NoError(t, err)
	assert.Equal(t, "虹の色よ鮮やかであれ (NELKE ver.)", track.Title)
	aliases, err = store.GetAllTrackAliases(ctx, track.ID)
	require.NoError(t, err)
	assert.Contains(t, utils.FlattenAliases(aliases), "Nijinoiroyo Azayakadeare (NELKE ver.)")
	// ensure track duration is saved
	assert.EqualValues(t, 218, track.Duration)

	artist, err = store.GetArtist(ctx, db.GetArtistOpts{MusicBrainzID: suzukiMBID})
	require.NoError(t, err)
	album, err = store.GetAlbum(ctx, db.GetAlbumOpts{ArtistID: artist.ID, Title: "GIRI GIRI"})
	require.NoError(t, err)
	_, err = store.GetTrack(ctx, db.GetTrackOpts{Title: "GIRI GIRI", ReleaseID: album.ID, ArtistIDs: []int32{artist.ID}})
	require.NoError(t, err)

	count, err := store.CountTracks(ctx, db.Timeframe{Period: db.PeriodAllTime})
	require.NoError(t, err)
	assert.EqualValues(t, 4, count)
	count, err = store.CountAlbums(ctx, db.Timeframe{Period: db.PeriodAllTime})
	require.NoError(t, err)
	assert.EqualValues(t, 3, count)
	count, err = store.CountArtists(ctx, db.Timeframe{Period: db.PeriodAllTime})
	require.NoError(t, err)
	assert.EqualValues(t, 6, count)
}

func TestImportYouTubeMusicFutureMusicBrainzScrobbles(t *testing.T) {
	for _, scenario := range []string{"mapped IDs", "unordered IDs", "unreachable", "same fallback album", "different real album", "different raw artist"} {
		t.Run(scenario, func(t *testing.T) {
			store := youtubeImportStore(t)
			ctx := youtubeImportContext()
			album := "Album"
			importedArtist := "Artist"
			if scenario == "different raw artist" {
				importedArtist = "An Uploader"
			}
			if strings.Contains(scenario, "album") {
				album = ""
			}
			youtubeImportCache(t, youtubeImportMetadata{VideoID: "aaaaaaaaaaa", Title: "Song", Artists: []string{importedArtist}, Album: album, RawArtist: scenario == "different raw artist", Sources: map[string]string{"title": "next", "artists": "next_artist"}})
			name := youtubeImportFile(t, youtubeImportHTML(youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist - Topic", "Jul 4, 2026, 1:02:03 PM CST")))
			require.NoError(t, importer.ImportYouTubeMusicFile(ctx, store, &mbz.MbzErrorCaller{}, name))
			r := youtubeImportReadReport(t, name)
			original, err := store.GetTrack(ctx, db.GetTrackOpts{ID: r.Records[0].TrackID})
			require.NoError(t, err)
			require.Nil(t, original.MbzID)
			artistID := original.Artists[0].ID
			artistMBID, albumMBID, recordingMBID := uuid.New(), uuid.New(), uuid.New()
			futureAlbum := album
			if scenario == "same fallback album" {
				futureAlbum = "Song"
			}
			if scenario == "different real album" {
				futureAlbum = "New Real Album"
			}
			var caller mbz.MusicBrainzCaller = &mbz.MbzMockCaller{
				Artists:  map[uuid.UUID]*mbz.MusicBrainzArtist{artistMBID: {Name: "Artist"}},
				Releases: map[uuid.UUID]*mbz.MusicBrainzRelease{albumMBID: {Title: futureAlbum}},
				Tracks:   map[uuid.UUID]*mbz.MusicBrainzTrack{recordingMBID: {Title: "Song", LengthMs: 204000}},
			}
			if scenario == "unreachable" {
				caller = &mbz.MbzErrorCaller{}
			}
			opts := catalog.SubmitListenOpts{MbzCaller: caller, Artist: "Artist", ArtistNames: []string{"Artist"}, TrackTitle: "Song", ReleaseTitle: futureAlbum,
				ArtistMbzIDs: []uuid.UUID{artistMBID}, ReleaseMbzID: albumMBID, RecordingMbzID: recordingMBID,
				Duration: 204, Time: r.Records[0].Time.Add(time.Hour), UserID: 1, Client: "future-client", SkipCacheImage: true}
			if scenario == "mapped IDs" {
				opts.ArtistMbidMappings = []catalog.ArtistMbidMap{{Artist: "Artist", Mbid: artistMBID}}
			}
			require.NoError(t, catalog.SubmitListen(ctx, store, opts))
			future, err := store.GetTrack(ctx, db.GetTrackOpts{MusicBrainzID: recordingMBID})
			require.NoError(t, err)
			assert.Equal(t, recordingMBID, *future.MbzID)
			if scenario == "different raw artist" {
				assert.NotEqual(t, artistID, future.Artists[0].ID)
			} else {
				assert.Equal(t, artistID, future.Artists[0].ID)
			}
			if scenario == "different real album" || scenario == "different raw artist" {
				assert.NotEqual(t, original.ID, future.ID)
				assert.NotEqual(t, original.AlbumID, future.AlbumID)
			} else {
				assert.Equal(t, original.ID, future.ID)
				assert.Equal(t, original.AlbumID, future.AlbumID)
				assert.EqualValues(t, 2, future.ListenCount)
				assert.EqualValues(t, 204, future.Duration)
			}
			artist, err := store.GetArtist(ctx, db.GetArtistOpts{ID: artistID})
			require.NoError(t, err)
			if scenario == "unreachable" || scenario == "different raw artist" {
				assert.Nil(t, artist.MbzID)
			} else {
				require.NotNil(t, artist.MbzID)
				assert.Equal(t, artistMBID, *artist.MbzID)
			}
			albumRecord, err := store.GetAlbum(ctx, db.GetAlbumOpts{ID: future.AlbumID})
			require.NoError(t, err)
			require.NotNil(t, albumRecord.MbzID)
			assert.Equal(t, albumMBID, *albumRecord.MbzID)
			count, err := store.Count(`SELECT COUNT(*) FROM listens`)
			require.NoError(t, err)
			assert.Equal(t, 2, count)
		})
	}
}

func TestImportYouTubeMusicExistingRecordingIDWins(t *testing.T) {
	store := youtubeImportStore(t)
	ctx := youtubeImportContext()
	youtubeImportCache(t, youtubeImportMetadata{VideoID: "aaaaaaaaaaa", Title: "Song", Artists: []string{"Artist"}, Album: "Imported Album", Sources: map[string]string{"title": "next", "artists": "next_artist"}})
	name := youtubeImportFile(t, youtubeImportHTML(youtubeImportCard("YouTube Music", "https://music.youtube.com/watch?v=aaaaaaaaaaa", "Song", "Artist", "Jul 4, 2026, 1:02:03 PM CST")))
	require.NoError(t, importer.ImportYouTubeMusicFile(ctx, store, &mbz.MbzErrorCaller{}, name))
	importedID := youtubeImportReadReport(t, name).Records[0].TrackID
	mbid := uuid.New()
	opts := catalog.SubmitListenOpts{MbzCaller: &mbz.MbzErrorCaller{}, Artist: "Artist", ArtistNames: []string{"Artist"}, TrackTitle: "Song", ReleaseTitle: "Other Album", RecordingMbzID: mbid, UserID: 1, Time: time.Date(2026, 7, 4, 20, 0, 0, 0, time.UTC)}
	require.NoError(t, catalog.SubmitListen(ctx, store, opts))
	existing, err := store.GetTrack(ctx, db.GetTrackOpts{MusicBrainzID: mbid})
	require.NoError(t, err)
	opts.ReleaseTitle, opts.Time = "Imported Album", opts.Time.Add(time.Hour)
	require.NoError(t, catalog.SubmitListen(ctx, store, opts))
	existing, err = store.GetTrack(ctx, db.GetTrackOpts{ID: existing.ID})
	require.NoError(t, err)
	assert.NotEqual(t, importedID, existing.ID)
	assert.EqualValues(t, 2, existing.ListenCount)
	imported, err := store.GetTrack(ctx, db.GetTrackOpts{ID: importedID})
	require.NoError(t, err)
	assert.Nil(t, imported.MbzID)
	assert.EqualValues(t, 1, imported.ListenCount)
}
