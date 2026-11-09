package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/gabehf/koito/internal/db"
)

// These CTEs shadow table names only for a website SELECT. Qualifying the raw
// tables with main prevents recursive references. Existing catalog views and
// unfiltered ingestion/export queries are never changed. Reusing the same query
// bodies keeps visibility before every aggregation, window function and LIMIT.
const webVisibilityCTEs = `
artists AS (
 SELECT a.* FROM main.artists a
 WHERE NOT EXISTS (SELECT 1 FROM main.hidden_artists h WHERE h.artist_id = a.id)
),
tracks AS (
 SELECT t.* FROM main.tracks t
 WHERE NOT EXISTS (
  SELECT 1 FROM main.artist_tracks at
  JOIN main.hidden_artists h ON h.artist_id = at.artist_id
  WHERE at.track_id = t.id
 )
),
listens AS (
 SELECT l.* FROM main.listens l JOIN tracks t ON t.id = l.track_id
),
releases AS (
 SELECT r.* FROM main.releases r
 WHERE EXISTS (SELECT 1 FROM tracks t WHERE t.release_id = r.id)
),
artist_tracks AS (
 SELECT at.* FROM main.artist_tracks at
 JOIN artists a ON a.id = at.artist_id JOIN tracks t ON t.id = at.track_id
),
artist_releases AS (
 SELECT ar.* FROM main.artist_releases ar
 JOIN artists a ON a.id = ar.artist_id JOIN releases r ON r.id = ar.release_id
),
artist_aliases AS (
 SELECT aa.* FROM main.artist_aliases aa JOIN artists a ON a.id = aa.artist_id
),
release_aliases AS (
 SELECT ra.* FROM main.release_aliases ra JOIN releases r ON r.id = ra.release_id
),
track_aliases AS (
 SELECT ta.* FROM main.track_aliases ta JOIN tracks t ON t.id = ta.track_id
),
artists_with_name AS (
 SELECT a.*, aa.alias AS name FROM artists a
 JOIN artist_aliases aa ON aa.artist_id = a.id WHERE aa.is_primary = 1
),
releases_with_title AS (
 SELECT r.*, ra.alias AS title FROM releases r
 JOIN release_aliases ra ON ra.release_id = r.id WHERE ra.is_primary = 1
),
tracks_with_title AS (
 SELECT t.*, ta.alias AS title FROM tracks t
 JOIN track_aliases ta ON ta.track_id = t.id WHERE ta.is_primary = 1
)
`

// database lets the web adapter scope SELECTs without changing catalog queries.
type database interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryRow(string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Exec(string, ...any) (sql.Result, error)
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	PingContext(context.Context) error
	Close() error
}

type webDatabase struct{ database }

func webQuery(query string) string {
	query = strings.TrimSpace(query)
	if rest, ok := strings.CutPrefix(query, "WITH "); ok {
		return "WITH " + webVisibilityCTEs + ", " + rest
	}
	return "WITH " + webVisibilityCTEs + " " + query
}

func (w *webDatabase) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return w.database.QueryContext(ctx, webQuery(query), args...)
}

func (w *webDatabase) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return w.database.QueryRowContext(ctx, webQuery(query), args...)
}

func (w *webDatabase) QueryRow(query string, args ...any) *sql.Row {
	return w.QueryRowContext(context.Background(), query, args...)
}

// Embed only read interfaces, not *Sqlite: no raw reads, writes, or exports can
// be obtained by a type assertion on the value returned by WebReads.
type webReadStore struct {
	db.ArtistReader
	db.AlbumReader
	db.TrackReader
	db.ListenReader
	db.ImageReader
	reader *Sqlite
}

func (s *Sqlite) WebReads() db.WebReadStore {
	reader := &Sqlite{db: &webDatabase{database: s.db}}
	return &webReadStore{reader, reader, reader, reader, reader, reader}
}

func (w *webReadStore) IsVisible(ctx context.Context, kind string, id int32) (bool, error) {
	var table string
	switch kind {
	case "artist":
		table = "artists"
	case "album":
		table = "releases"
	case "track":
		table = "tracks"
	default:
		return false, fmt.Errorf("unknown media type %q", kind)
	}
	var visible bool
	err := w.reader.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id = ?)", id).Scan(&visible)
	return visible, err
}

func (w *webReadStore) WebsiteRevision(ctx context.Context) (string, error) {
	var revision int64
	err := w.reader.db.QueryRowContext(ctx, "SELECT revision FROM website_revision WHERE id = 1").Scan(&revision)
	// Include the pool identity because the process cache can serve multiple stores.
	return fmt.Sprintf("%p_%d", w.reader.db, revision), err
}

// A website with only hidden history has no first-activity date.
func (w *webReadStore) GetFirstListenUnix(ctx context.Context) (int64, error) {
	var first sql.NullInt64
	err := w.reader.db.QueryRowContext(ctx, "SELECT MIN(listened_at) FROM listens").Scan(&first)
	return first.Int64, err
}
