package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/gabehf/koito/internal/db"
)

func (s *Sqlite) GetHiddenArtists(ctx context.Context) ([]db.ArtistVisibility, error) {
	return s.visibilityArtists(ctx, "WHERE h.artist_id IS NOT NULL", nil)
}

func (s *Sqlite) SearchArtistsForVisibility(ctx context.Context, query string) ([]db.ArtistVisibility, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []db.ArtistVisibility{}, nil
	}
	// Escape LIKE metacharacters so names containing % or _ remain literal.
	query = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	return s.visibilityArtists(ctx, `WHERE EXISTS (
  SELECT 1 FROM artist_aliases aa WHERE aa.artist_id = a.id AND aa.alias LIKE ? ESCAPE '\'
 )`, []any{"%" + query + "%"})
}

func (s *Sqlite) visibilityArtists(ctx context.Context, where string, args []any) ([]db.ArtistVisibility, error) {
	limit := ""
	if len(args) > 0 {
		limit = " LIMIT 30"
	}
	rows, err := s.db.QueryContext(ctx, `
 SELECT a.id, awn.name, a.musicbrainz_id, h.artist_id IS NOT NULL
 FROM artists a JOIN artists_with_name awn ON awn.id = a.id
 LEFT JOIN hidden_artists h ON h.artist_id = a.id `+where+` ORDER BY awn.name COLLATE NOCASE, a.id`+limit, args...)
	if err != nil {
		return nil, fmt.Errorf("visibilityArtists: %w", err)
	}
	defer rows.Close()
	result := make([]db.ArtistVisibility, 0)
	for rows.Next() {
		var artist db.ArtistVisibility
		var mbid sql.NullString
		if err := rows.Scan(&artist.ID, &artist.Name, &mbid, &artist.Hidden); err != nil {
			return nil, err
		}
		artist.MusicBrainzID = parseNullableUUID(mbid)
		result = append(result, artist)
	}
	return result, rows.Err()
}

func (s *Sqlite) SetArtistHidden(ctx context.Context, id int32, hidden bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM artists WHERE id = ?)", id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return db.ErrNotFound
	}
	if hidden {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO hidden_artists (artist_id) VALUES (?)", id)
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM hidden_artists WHERE artist_id = ?", id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
