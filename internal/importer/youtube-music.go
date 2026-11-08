package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gabehf/koito/internal/catalog"
	"github.com/gabehf/koito/internal/cfg"
	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/logger"
	"github.com/gabehf/koito/internal/mbz"
	"golang.org/x/net/html"
	"golang.org/x/time/rate"
)

var youtubeVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type youtubeMusicEntry struct {
	Index     int       `json:"activity_index"`
	URL       string    `json:"url,omitempty"`
	VideoID   string    `json:"video_id,omitempty"`
	Title     string    `json:"raw_title,omitempty"`
	Channel   string    `json:"raw_channel,omitempty"`
	Timestamp string    `json:"raw_timestamp,omitempty"`
	Time      time.Time `json:"time"`
	Invalid   string    `json:"validation_error,omitempty"`
}

type youtubeMusicMetadata struct {
	VideoID   string            `json:"video_id"`
	Title     string            `json:"title"`
	Artists   []string          `json:"artists"`
	Album     string            `json:"album,omitempty"`
	Duration  int32             `json:"duration_seconds"`
	Sources   map[string]string `json:"sources"`
	RawArtist bool              `json:"uploader_fallback,omitempty"`
}

type youtubeMusicRecord struct {
	youtubeMusicEntry
	Metadata      *youtubeMusicMetadata `json:"metadata,omitempty"`
	MissingFields []string              `json:"missing_fields,omitempty"`
	AlbumFallback bool                  `json:"album_uses_track_title,omitempty"`
	Quality       string                `json:"metadata_quality,omitempty"`
	TrackID       int32                 `json:"track_id,omitempty"`
	Outcome       string                `json:"outcome"`
	Reason        string                `json:"reason,omitempty"`
}

type youtubeMusicReport struct {
	Version      int                  `json:"version"`
	Filename     string               `json:"filename"`
	SHA256       string               `json:"sha256"`
	StartedAt    time.Time            `json:"started_at"`
	FinishedAt   time.Time            `json:"finished_at"`
	Complete     bool                 `json:"complete"`
	Error        string               `json:"error,omitempty"`
	Activities   int                  `json:"activities"`
	Excluded     int                  `json:"excluded_activities"`
	MusicRecords int                  `json:"music_records"`
	UniqueVideos int                  `json:"unique_videos"`
	Outcomes     map[string]int       `json:"outcomes"`
	Qualities    map[string]int       `json:"metadata_qualities"`
	Records      []youtubeMusicRecord `json:"records"`
}

type youtubeMusicResolver interface {
	Resolve(context.Context, string) (youtubeMusicMetadata, error)
}

// ImportYouTubeMusicFile imports an English Google Takeout HTML watch history.
// It only submits Music activity; ordinary YouTube watches remain excluded even
// if the video is a song or its title mentions YouTube Music.
func ImportYouTubeMusicFile(ctx context.Context, store importStore, mbzc mbz.MusicBrainzCaller, filename string) error {
	return importYouTubeMusicFile(ctx, store, mbzc, filename, newYouTubeMusicClient())
}

func importYouTubeMusicFile(ctx context.Context, store importStore, mbzc mbz.MusicBrainzCaller, filename string, resolver youtubeMusicResolver) (retErr error) {
	startedAt := time.Now().UTC()
	if filepath.Base(filename) != filename || filename == "." {
		return fmt.Errorf("YouTube Music: invalid filename %q", filename)
	}
	l := logger.FromContext(ctx)
	l.Info().Msgf("Beginning YouTube Music import: %s", filename)
	f, err := os.Open(filepath.Join(cfg.ConfigDir(), "import", filename))
	if err != nil {
		return fmt.Errorf("YouTube Music: open history: %w", err)
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return fmt.Errorf("YouTube Music: hash history: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	report, parseErr := parseYouTubeMusicHistory(ctx, f)
	report.Version = 1
	report.Filename = filename
	report.SHA256 = hex.EncodeToString(hash.Sum(nil))
	report.StartedAt = startedAt
	reportPath := filepath.Join(cfg.ConfigDir(), "import_reports", filename+"."+report.SHA256+".youtube-music.json")
	// Persist failure diagnostics even when the request context was canceled.
	// Successful reports are written before archiving below.
	defer func() {
		report.FinishedAt = time.Now().UTC()
		if retErr != nil {
			report.Error = retErr.Error()
			report.Complete = false
		}
		report.summarize()
		if retErr != nil {
			retErr = errors.Join(retErr, writeYouTubeMusicJSON(reportPath, report))
		}
		l.Info().Int("music_records", report.MusicRecords).Int("unique_videos", report.UniqueVideos).
			Any("outcomes", report.Outcomes).Any("metadata_qualities", report.Qualities).
			Str("report", reportPath).Msg("YouTube Music import reconciliation")
	}()
	if parseErr != nil {
		return fmt.Errorf("YouTube Music: parse history: %w", parseErr)
	}
	cache := make(map[string]youtubeMusicMetadata)
	cacheDir := filepath.Join(cfg.ConfigDir(), "youtube-music", "metadata-v1")
	countingStore := &youtubeMusicCountingStore{importStore: store}
	for i := range report.Records {
		r := &report.Records[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Invalid != "" {
			r.Outcome, r.Reason = "invalid", r.Invalid
			continue
		}
		if !inImportTimeWindow(r.Time) {
			r.Outcome, r.Reason = "outside_window", "excluded by import time window"
			continue
		}
		meta, ok := cache[r.VideoID]
		if !ok {
			cachePath := filepath.Join(cacheDir, r.VideoID+".json")
			data, err := os.ReadFile(cachePath)
			switch {
			case err == nil:
				if err := json.Unmarshal(data, &meta); err != nil || !meta.valid(r.VideoID) {
					return fmt.Errorf("YouTube Music: invalid metadata cache %s", cachePath)
				}
			case errors.Is(err, os.ErrNotExist):
				meta, err = resolver.Resolve(ctx, r.VideoID)
				if err != nil {
					r.Reason = err.Error()
					return fmt.Errorf("YouTube Music: resolve %s: %w", r.VideoID, err)
				}
				if meta.VideoID != r.VideoID {
					return fmt.Errorf("YouTube Music: resolver returned metadata for a different video")
				}
				meta.withHistory(r.youtubeMusicEntry)
				if meta.valid(r.VideoID) {
					// Pin the effective identity before any catalog writes so a
					// restart cannot reimport it under refreshed metadata.
					if err := writeYouTubeMusicJSON(cachePath, meta); err != nil {
						return fmt.Errorf("YouTube Music: persist metadata: %w", err)
					}
				}
			default:
				return fmt.Errorf("YouTube Music: read metadata cache: %w", err)
			}
			cache[r.VideoID] = meta
		}
		// A later occurrence can supply HTML fields missing from the first
		// occurrence of an unresolved ID. Already usable identities stay pinned.
		if ok && !meta.valid(r.VideoID) {
			meta.Sources = maps.Clone(meta.Sources)
			meta.withHistory(r.youtubeMusicEntry)
			if meta.valid(r.VideoID) {
				if err := writeYouTubeMusicJSON(filepath.Join(cacheDir, r.VideoID+".json"), meta); err != nil {
					return fmt.Errorf("YouTube Music: persist history fallback: %w", err)
				}
				cache[r.VideoID] = meta
			}
		}
		r.Metadata = &meta
		r.MissingFields, r.Quality = meta.quality()
		r.AlbumFallback = meta.Album == ""
		if !meta.valid(r.VideoID) {
			r.Outcome, r.Reason = "unresolved", "no usable track title or artist/channel after metadata lookup and history fallback"
			continue
		}
		countingStore.submitted, countingStore.trackID = false, 0
		err := catalog.SubmitListen(ctx, countingStore, catalog.SubmitListenOpts{
			MbzCaller: mbzc, Artist: meta.Artists[0], ArtistNames: meta.Artists,
			TrackTitle: meta.Title, ReleaseTitle: meta.Album, Duration: meta.Duration,
			Time: r.Time, Client: "youtube-music", UserID: 1,
			SkipCacheImage: !cfg.FetchImagesDuringImport(),
		})
		if err != nil {
			r.Reason = err.Error()
			return fmt.Errorf("YouTube Music: submit activity %d: %w", r.Index, err)
		}
		r.TrackID = countingStore.trackID
		r.Outcome = "already_present"
		if countingStore.submitted {
			r.Outcome = "submitted"
		}
		if ms := cfg.ThrottleImportMs(); ms > 0 {
			if err := waitYouTubeMusic(ctx, time.Duration(ms)*time.Millisecond); err != nil {
				return err
			}
		}
		if (i+1)%100 == 0 {
			l.Info().Int("processed", i+1).Int("total", len(report.Records)).Msg("YouTube Music import progress")
		}
	}
	// A report must exist before the source leaves the pending directory.
	report.Complete = true
	report.FinishedAt = time.Now().UTC()
	report.summarize()
	if err := writeYouTubeMusicJSON(reportPath, report); err != nil {
		return fmt.Errorf("YouTube Music: save report before archival: %w", err)
	}
	// Completion errors are local to this importer; other importers keep their
	// existing completion behavior.
	if err := f.Close(); err != nil {
		return fmt.Errorf("YouTube Music: close history before archival: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.ConfigDir(), "import_complete"), 0744); err != nil {
		return fmt.Errorf("YouTube Music: create archive directory: %w", err)
	}
	if err := os.Rename(filepath.Join(cfg.ConfigDir(), "import", filename), filepath.Join(cfg.ConfigDir(), "import_complete", filename)); err != nil {
		return fmt.Errorf("YouTube Music: archive history: %w", err)
	}
	return nil
}

// Intercept catalog submissions using the existing store interface. The report
// distinguishes keys already present at lookup from accepted submissions, not
// atomic insert ownership: SaveListen intentionally does not return affected rows.
type youtubeMusicCountingStore struct {
	importStore
	submitted bool
	trackID   int32
}

func (s *youtubeMusicCountingStore) SaveListen(ctx context.Context, opts db.SaveListenOpts) error {
	s.trackID = opts.TrackID
	s.submitted = false
	existing, err := s.importStore.CountListensToItem(ctx, db.TimeListenedOpts{
		TrackID:   opts.TrackID,
		Timeframe: db.Timeframe{From: opts.Time, To: opts.Time},
	})
	if err != nil {
		return fmt.Errorf("YouTube Music: check existing listen: %w", err)
	}
	if err := s.importStore.SaveListen(ctx, opts); err != nil {
		return err
	}
	s.submitted = existing == 0
	return nil
}

func (m *youtubeMusicMetadata) withHistory(e youtubeMusicEntry) {
	if m.Sources == nil {
		m.Sources = make(map[string]string)
	}
	if !usableYouTubeMusicTitle(m.Title, e.VideoID) {
		m.Title = ""
		if usableYouTubeMusicTitle(e.Title, e.VideoID) {
			m.Title, m.Sources["title"] = e.Title, "history"
		}
	}
	if len(m.Artists) == 0 && cleanYouTubeMusicChannel(e.Channel) != "" {
		m.Artists = []string{cleanYouTubeMusicChannel(e.Channel)}
		m.RawArtist, m.Sources["artists"] = true, "history_channel"
	}
}

func cleanYouTubeMusicChannel(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), " - Topic"))
}

func usableYouTubeMusicTitle(s, videoID string) bool {
	s = strings.TrimSpace(s)
	return s != "" && s != videoID && !strings.HasPrefix(strings.ToLower(s), "https://") && !strings.HasPrefix(strings.ToLower(s), "http://")
}

func (m youtubeMusicMetadata) valid(videoID string) bool {
	return m.VideoID == videoID && youtubeVideoID.MatchString(videoID) && usableYouTubeMusicTitle(m.Title, videoID) &&
		len(m.Artists) > 0 && !slices.ContainsFunc(m.Artists, func(a string) bool { return strings.TrimSpace(a) == "" }) && m.Duration >= 0 && m.Sources != nil
}

func (m youtubeMusicMetadata) quality() ([]string, string) {
	var missing []string
	if !usableYouTubeMusicTitle(m.Title, m.VideoID) {
		missing = append(missing, "title")
	}
	if len(m.Artists) == 0 {
		missing = append(missing, "artists")
	}
	if m.Album == "" {
		missing = append(missing, "album")
	}
	if m.Duration == 0 {
		missing = append(missing, "duration")
	}
	quality := "resolved"
	if len(missing) > 0 {
		quality = "partial"
	}
	if m.RawArtist || m.Sources["title"] == "history" || m.Sources["title"] == "oembed" {
		quality = "raw"
	}
	return missing, quality
}

func (r *youtubeMusicReport) summarize() {
	r.MusicRecords = len(r.Records)
	unique := make(map[string]bool)
	r.Outcomes = map[string]int{"submitted": 0, "already_present": 0, "outside_window": 0, "invalid": 0, "unresolved": 0, "pending": 0}
	r.Qualities = make(map[string]int)
	for _, record := range r.Records {
		if record.VideoID != "" {
			unique[record.VideoID] = true
		}
		r.Outcomes[record.Outcome]++
		if record.Quality != "" {
			r.Qualities[record.Quality]++
		}
	}
	r.UniqueVideos = len(unique)
}

// Temp files live beside the destination, so rename stays on one filesystem.
func writeYouTubeMusicJSON(filename string, value any) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".youtube-music-*")
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	enc := json.NewEncoder(f)
	if err := enc.Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filename)
}

type youtubeMusicLink struct{ href, text string }

// Google Takeout uses div-based activity cards. Track depth and only retain the
// current card, rather than constructing a DOM for the entire export.
func parseYouTubeMusicHistory(ctx context.Context, reader io.Reader) (youtubeMusicReport, error) {
	report := youtubeMusicReport{Records: make([]youtubeMusicRecord, 0)}
	z := html.NewTokenizer(reader)
	z.SetMaxBuf(4 << 20)
	depth, outer, header, content := 0, 0, 0, 0
	var heading, body strings.Builder
	var links []youtubeMusicLink
	link := -1
	closedBody := false
	unique := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); err != io.EOF {
				return report, err
			}
			if outer != 0 || depth != 0 || !closedBody {
				return report, errors.New("truncated or unrecognized HTML history")
			}
			report.MusicRecords, report.UniqueVideos = len(report.Records), len(unique)
			return report, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "div" {
				depth++
				classes := ""
				for _, a := range t.Attr {
					if a.Key == "class" {
						classes = " " + strings.Join(strings.Fields(a.Val), " ") + " "
					}
				}
				has := func(c string) bool { return strings.Contains(classes, " "+c+" ") }
				if has("outer-cell") {
					if outer != 0 {
						return report, errors.New("nested activity cards")
					}
					outer = depth
					report.Activities++
					heading.Reset()
					body.Reset()
					links = nil
					link = -1
					header = 0
					content = 0
				}
				if outer != 0 && has("header-cell") {
					header = depth
				}
				if outer != 0 && content == 0 && has("content-cell") && !has("mdl-typography--caption") && !has("mdl-typography--text-right") {
					content = depth
				}
			}
			if content > 0 && depth >= content && t.Data == "a" {
				for _, a := range t.Attr {
					if a.Key == "href" {
						links = append(links, youtubeMusicLink{href: a.Val})
						link = len(links) - 1
					}
				}
			}
			if t.Data == "br" {
				if header > 0 {
					heading.WriteByte('\n')
				}
				if content > 0 {
					body.WriteByte('\n')
				}
			}
		case html.TextToken:
			text := string(z.Text())
			if header > 0 {
				heading.WriteString(text)
			}
			if content > 0 {
				body.WriteString(text)
				if link >= 0 {
					links[link].text += text
				}
			}
		case html.EndTagToken:
			t := z.Token()
			if t.Data == "body" {
				closedBody = true
			}
			if t.Data == "a" {
				link = -1
			}
			if t.Data != "div" {
				continue
			}
			if outer == depth && outer != 0 {
				entry, music := youtubeMusicCard(report.Activities, strings.TrimSpace(heading.String()), body.String(), links)
				if music {
					report.Records = append(report.Records, youtubeMusicRecord{youtubeMusicEntry: entry, Outcome: "pending"})
					if entry.VideoID != "" {
						unique[entry.VideoID] = true
					}
				} else {
					report.Excluded++
				}
				outer = 0
			}
			if header == depth {
				header = 0
			}
			// -1 means the primary cell has ended; never parse captions as a
			// second content cell. Reset this sentinel at the next card.
			if content == depth {
				content = -1
			}
			depth--
			if depth < 0 {
				return report, errors.New("unbalanced activity HTML")
			}
		}
	}
}

func youtubeMusicCard(index int, heading, body string, links []youtubeMusicLink) (youtubeMusicEntry, bool) {
	e := youtubeMusicEntry{Index: index}
	// Keep the original diagnostic fields even when the card is invalid.
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			e.Timestamp = line
		}
	}
	musicHeading := strings.Join(strings.Fields(heading), " ") == "YouTube Music"
	var watchLinks []youtubeMusicLink
	musicLink := false
	for _, a := range links {
		u, err := url.Parse(a.href)
		if err != nil {
			continue
		}
		if u.Path == "/watch" {
			watchLinks = append(watchLinks, a)
			musicLink = musicLink || strings.EqualFold(u.Hostname(), "music.youtube.com")
		}
		if (u.Hostname() == "www.youtube.com" || u.Hostname() == "youtube.com") && strings.HasPrefix(u.Path, "/channel/") {
			e.Channel = strings.TrimSpace(a.text)
		}
	}
	if !musicHeading && !musicLink {
		return e, false
	}
	if len(watchLinks) > 0 {
		e.URL, e.Title = watchLinks[0].href, strings.TrimSpace(watchLinks[0].text)
	}
	if !musicHeading || !musicLink || len(watchLinks) != 1 {
		e.Invalid = "missing or contradictory YouTube Music header/watch link"
		return e, true
	}
	a := watchLinks[0]
	u, _ := url.Parse(a.href)
	ids := u.Query()["v"]
	if u.User != nil || u.Port() != "" || (u.Scheme != "https" && u.Scheme != "http") || len(ids) != 1 || !youtubeVideoID.MatchString(ids[0]) {
		e.Invalid = "invalid YouTube Music watch URL"
		return e, true
	}
	e.VideoID = ids[0]
	if !strings.HasPrefix(strings.Join(strings.Fields(body), " "), "Watched ") {
		e.Invalid = "unrecognized watch action (expected an English Watched activity)"
		return e, true
	}
	var err error
	e.Time, err = parseYouTubeMusicTime(e.Timestamp)
	if err != nil {
		e.Invalid = err.Error()
	}
	return e, true
}

func parseYouTubeMusicTime(s string) (time.Time, error) {
	s = strings.Join(strings.Fields(s), " ")
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return time.Time{}, errors.New("missing watch timestamp")
	}
	zone := fields[len(fields)-1]
	offsets := map[string]int{"UTC": 0, "GMT": 0, "EST": -5, "EDT": -4, "CST": -6, "CDT": -5, "MST": -7, "MDT": -6, "PST": -8, "PDT": -7}
	if offset, ok := offsets[zone]; ok {
		t, err := time.ParseInLocation("Jan 2, 2006, 3:04:05 PM MST", s, time.FixedZone(zone, offset*3600))
		if err == nil {
			return t.UTC(), nil
		}
	} else {
		for _, layout := range []string{"Jan 2, 2006, 3:04:05 PM -07:00", "Jan 2, 2006, 3:04:05 PM -0700"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid or unsupported watch timestamp %q", s)
}

const youtubeMusicUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

var errYouTubeMusicUnavailable = errors.New("YouTube metadata unavailable")

type youtubeMusicClient struct {
	http      *http.Client
	musicURL  string
	oembedURL string
	limiter   *rate.Limiter
	maxBytes  int64
	wait      func(context.Context, time.Duration) error
	version   string
	apiKey    string
	visitor   string
}

func newYouTubeMusicClient() *youtubeMusicClient {
	return &youtubeMusicClient{
		http:     &http.Client{Timeout: 20 * time.Second},
		musicURL: "https://music.youtube.com", oembedURL: "https://www.youtube.com/oembed",
		limiter: rate.NewLimiter(rate.Every(time.Second), 1), maxBytes: 8 << 20,
		wait: waitYouTubeMusic,
	}
}

func waitYouTubeMusic(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Resolve only reads public metadata. It never requests audio, authenticates,
// follows playback tracking URLs, or modifies a user's YouTube history.
func (c *youtubeMusicClient) Resolve(ctx context.Context, id string) (youtubeMusicMetadata, error) {
	m := youtubeMusicMetadata{VideoID: id, Sources: make(map[string]string)}
	if !youtubeVideoID.MatchString(id) {
		return m, errors.New("invalid YouTube video ID")
	}
	if c.version == "" {
		if err := c.bootstrap(ctx); err != nil {
			return m, err
		}
	}
	body, err := c.api(ctx, "next", map[string]any{
		"videoId": id, "playlistId": "RDAMVM" + id,
		"enablePersistentPlaylistPanel": true, "isAudioOnly": true,
		"tunerSettingValue": "AUTOMIX_SETTING_NORMAL",
		"watchEndpointMusicSupportedConfigs": map[string]any{
			"watchEndpointMusicConfig": map[string]any{"hasPersistentPlaylistPanel": true, "musicVideoType": "MUSIC_VIDEO_TYPE_ATV"},
		},
	})
	if err == nil {
		if err := parseYouTubeMusicNext(body, &m); err != nil {
			return m, err
		}
	} else if !errors.Is(err, errYouTubeMusicUnavailable) {
		return m, err
	}
	if !usableYouTubeMusicTitle(m.Title, id) || len(m.Artists) == 0 || m.Duration == 0 {
		body, err = c.api(ctx, "player", map[string]any{"videoId": id, "video_id": id})
		if err == nil {
			if err := parseYouTubeMusicPlayer(body, &m); err != nil {
				return m, err
			}
		} else if !errors.Is(err, errYouTubeMusicUnavailable) {
			return m, err
		}
	}
	if !usableYouTubeMusicTitle(m.Title, id) || len(m.Artists) == 0 {
		q := url.Values{"format": {"json"}, "url": {"https://www.youtube.com/watch?v=" + id}}
		body, err = c.request(ctx, http.MethodGet, c.oembedURL+"?"+q.Encode(), nil)
		if err == nil {
			var o struct {
				Title  string `json:"title"`
				Author string `json:"author_name"`
			}
			if err := json.Unmarshal(body, &o); err != nil {
				return m, fmt.Errorf("invalid YouTube oEmbed response: %w", err)
			}
			if !usableYouTubeMusicTitle(m.Title, id) && usableYouTubeMusicTitle(o.Title, id) {
				m.Title, m.Sources["title"] = strings.TrimSpace(o.Title), "oembed"
			}
			if len(m.Artists) == 0 && cleanYouTubeMusicChannel(o.Author) != "" {
				m.Artists = []string{cleanYouTubeMusicChannel(o.Author)}
				m.RawArtist, m.Sources["artists"] = true, "oembed_channel"
			}
		} else if !errors.Is(err, errYouTubeMusicUnavailable) {
			return m, err
		}
	}
	return m, nil
}

func (c *youtubeMusicClient) bootstrap(ctx context.Context) error {
	body, err := c.request(ctx, http.MethodGet, c.musicURL+"/", nil)
	if err != nil {
		return fmt.Errorf("bootstrap YouTube Music: %w", err)
	}
	// Values are JSON strings inside ytcfg.set; decoding the captured string
	// handles escaped visitor data without executing any page JavaScript.
	value := func(key string) string {
		re := regexp.MustCompile(`"` + key + `"\s*:\s*("(?:[^"\\]|\\.)*")`)
		match := re.FindSubmatch(body)
		var s string
		if len(match) == 2 {
			_ = json.Unmarshal(match[1], &s)
		}
		return s
	}
	c.version, c.apiKey, c.visitor = value("INNERTUBE_CLIENT_VERSION"), value("INNERTUBE_API_KEY"), value("VISITOR_DATA")
	if c.version == "" {
		return errors.New("YouTube Music bootstrap has no WEB_REMIX version (consent page or changed response)")
	}
	return nil
}

func (c *youtubeMusicClient) api(ctx context.Context, endpoint string, body map[string]any) ([]byte, error) {
	body["context"] = map[string]any{"client": map[string]any{
		"clientName": "WEB_REMIX", "clientVersion": c.version, "hl": "en", "gl": "US",
	}}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	q := url.Values{"prettyPrint": {"false"}}
	if c.apiKey != "" {
		q.Set("key", c.apiKey)
	}
	return c.request(ctx, http.MethodPost, c.musicURL+"/youtubei/v1/"+endpoint+"?"+q.Encode(), encoded)
}

func (c *youtubeMusicClient) request(ctx context.Context, method, address string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", youtubeMusicUserAgent)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", c.musicURL)
			req.Header.Set("Referer", c.musicURL+"/")
			if c.visitor != "" {
				req.Header.Set("X-Goog-Visitor-Id", c.visitor)
			}
		}
		resp, err := c.http.Do(req)
		delay := time.Second * time.Duration(1<<attempt)
		if err != nil {
			lastErr = fmt.Errorf("YouTube metadata request failed: %w", err)
		} else {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
			resp.Body.Close()
			if int64(len(data)) > c.maxBytes {
				return nil, errors.New("YouTube metadata response exceeds size limit")
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && readErr == nil {
				return data, nil
			}
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
				return nil, errYouTubeMusicUnavailable
			}
			if readErr != nil {
				lastErr = fmt.Errorf("read YouTube metadata: %w", readErr)
			} else {
				lastErr = fmt.Errorf("YouTube metadata HTTP status %d", resp.StatusCode)
				if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode < 500 {
					return nil, lastErr
				}
			}
			if seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); err == nil && seconds >= 0 {
				delay = max(delay, time.Duration(seconds)*time.Second)
			} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
				delay = max(delay, time.Until(date))
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt < 2 {
			if err := c.wait(ctx, delay); err != nil {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

type youtubeMusicText struct {
	Simple string `json:"simpleText"`
	Runs   []struct {
		Text       string `json:"text"`
		Navigation struct {
			Browse struct {
				ID      string `json:"browseId"`
				Configs struct {
					Music struct {
						PageType string `json:"pageType"`
					} `json:"browseEndpointContextMusicConfig"`
				} `json:"browseEndpointContextSupportedConfigs"`
			} `json:"browseEndpoint"`
		} `json:"navigationEndpoint"`
	} `json:"runs"`
}

func (t youtubeMusicText) text() string {
	if t.Simple != "" {
		return strings.TrimSpace(t.Simple)
	}
	var b strings.Builder
	for _, r := range t.Runs {
		b.WriteString(r.Text)
	}
	return strings.TrimSpace(b.String())
}

// Decode only the documented queue path; recursive searches across the whole
// response can accidentally select recommendations or unrelated UI metadata.
func parseYouTubeMusicNext(data []byte, m *youtubeMusicMetadata) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("invalid YouTube Music next response: %w", err)
	}
	if _, ok := root["error"]; ok {
		return errors.New("YouTube Music next returned an API error")
	}
	tabs, ok := youtubeMusicJSONPath(data, "contents", "singleColumnMusicWatchNextResultsRenderer", "tabbedRenderer", "watchNextTabbedResultsRenderer", "tabs")
	if !ok {
		return errors.New("unrecognized YouTube Music next response: missing watch tabs")
	}
	var tabList []json.RawMessage
	if err := json.Unmarshal(tabs, &tabList); err != nil {
		return fmt.Errorf("invalid YouTube Music watch tabs: %w", err)
	}
	for _, tab := range tabList {
		queue, ok := youtubeMusicJSONPath(tab, "tabRenderer", "content", "musicQueueRenderer", "content", "playlistPanelRenderer", "contents")
		if !ok {
			continue
		} // unavailable queues and lyrics/related tabs
		var items []json.RawMessage
		if err := json.Unmarshal(queue, &items); err != nil {
			return fmt.Errorf("invalid YouTube Music queue: %w", err)
		}
		recognizedVideo := false
		for _, item := range items {
			var candidates []json.RawMessage
			if v, ok := youtubeMusicJSONPath(item, "playlistPanelVideoRenderer"); ok {
				candidates = append(candidates, v)
			}
			if wrapper, ok := youtubeMusicJSONPath(item, "playlistPanelVideoWrapperRenderer"); ok {
				if v, ok := youtubeMusicJSONPath(wrapper, "primaryRenderer", "playlistPanelVideoRenderer"); ok {
					candidates = append(candidates, v)
				}
				if raw, ok := youtubeMusicJSONPath(wrapper, "counterpart"); ok {
					var counterparts []json.RawMessage
					if err := json.Unmarshal(raw, &counterparts); err != nil {
						return fmt.Errorf("invalid YouTube Music counterpart: %w", err)
					}
					for _, counterpart := range counterparts {
						if v, ok := youtubeMusicJSONPath(counterpart, "counterpartRenderer", "playlistPanelVideoRenderer"); ok {
							candidates = append(candidates, v)
						}
					}
				}
			}
			for _, raw := range candidates {
				recognizedVideo = true
				var v struct {
					ID     string           `json:"videoId"`
					Title  youtubeMusicText `json:"title"`
					Byline youtubeMusicText `json:"longBylineText"`
					Length youtubeMusicText `json:"lengthText"`
				}
				if err := json.Unmarshal(raw, &v); err != nil {
					return fmt.Errorf("invalid YouTube Music queue video: %w", err)
				}
				if v.ID != m.VideoID {
					continue
				}
				if title := v.Title.text(); usableYouTubeMusicTitle(title, m.VideoID) {
					m.Title, m.Sources["title"] = title, "next"
				}
				var channels []string
				for _, run := range v.Byline.Runs {
					name := strings.TrimSpace(run.Text)
					if name == "" {
						continue
					}
					switch run.Navigation.Browse.Configs.Music.PageType {
					case "MUSIC_PAGE_TYPE_ARTIST":
						if !containsYouTubeMusicArtist(m.Artists, name) {
							m.Artists = append(m.Artists, name)
						}
					case "MUSIC_PAGE_TYPE_ALBUM":
						m.Album, m.Sources["album"] = name, "next"
					case "MUSIC_PAGE_TYPE_USER_CHANNEL":
						channels = append(channels, name)
					}
				}
				if len(m.Artists) > 0 {
					m.Sources["artists"] = "next_artist"
				} else if len(channels) > 0 {
					m.Artists, m.RawArtist, m.Sources["artists"] = channels, true, "next_channel"
				}
				if d := parseYouTubeMusicDuration(v.Length.text()); d > 0 {
					m.Duration, m.Sources["duration"] = d, "next"
				}
				return nil
			}
		}
		if len(items) > 0 && !recognizedVideo {
			return errors.New("unrecognized YouTube Music queue video renderer")
		}
	}
	return nil
}

func parseYouTubeMusicPlayer(data []byte, m *youtubeMusicMetadata) error {
	var p struct {
		Details struct {
			ID     string `json:"videoId"`
			Title  string `json:"title"`
			Author string `json:"author"`
			Length string `json:"lengthSeconds"`
			Type   string `json:"musicVideoType"`
		} `json:"videoDetails"`
		Status struct {
			Status string `json:"status"`
		} `json:"playabilityStatus"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("invalid YouTube Music player response: %w", err)
	}
	if p.Details.ID == "" {
		if p.Status.Status == "ERROR" || p.Status.Status == "UNPLAYABLE" || p.Status.Status == "LOGIN_REQUIRED" {
			return nil
		}
		return errors.New("unrecognized YouTube Music player response: no video identity or unavailable status")
	}
	if p.Details.ID != m.VideoID {
		return errors.New("YouTube Music player returned a different video ID")
	}
	if !usableYouTubeMusicTitle(m.Title, m.VideoID) && usableYouTubeMusicTitle(p.Details.Title, m.VideoID) {
		m.Title, m.Sources["title"] = strings.TrimSpace(p.Details.Title), "player"
	}
	if len(m.Artists) == 0 && cleanYouTubeMusicChannel(p.Details.Author) != "" {
		m.Artists = []string{cleanYouTubeMusicChannel(p.Details.Author)}
		m.RawArtist = p.Details.Type != "MUSIC_VIDEO_TYPE_ATV" && p.Details.Type != "MUSIC_VIDEO_TYPE_OMV"
		m.Sources["artists"] = "player_author"
	}
	if seconds, err := strconv.ParseInt(p.Details.Length, 10, 32); err == nil && seconds > 0 {
		m.Duration, m.Sources["duration"] = int32(seconds), "player"
	}
	return nil
}

func youtubeMusicJSONPath(data json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil {
			return nil, false
		}
		var ok bool
		data, ok = object[key]
		if !ok || string(data) == "null" {
			return nil, false
		}
	}
	return data, true
}

func parseYouTubeMusicDuration(s string) int32 {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var total int64
	for i, part := range parts {
		n, err := strconv.ParseInt(part, 10, 32)
		if err != nil || n < 0 || (i > 0 && n >= 60) {
			return 0
		}
		total = total*60 + n
		if total > math.MaxInt32 {
			return 0
		}
	}
	return int32(total)
}

func containsYouTubeMusicArtist(artists []string, name string) bool {
	for _, a := range artists {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}
