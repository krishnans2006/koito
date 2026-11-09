package db

import (
	"context"
	"github.com/google/uuid"
)

// WebReadStore deliberately exposes neither catalog writes nor exports. Normal
// website handlers must use this view; ingestion always uses the unfiltered DB.
type WebReadStore interface {
	ArtistReader
	AlbumReader
	TrackReader
	ListenReader
	ImageReader
	IsVisible(ctx context.Context, kind string, id int32) (bool, error)
	WebsiteRevision(ctx context.Context) (string, error)
}

type ArtistVisibility struct {
	ID            int32      `json:"id"`
	Name          string     `json:"name"`
	MusicBrainzID *uuid.UUID `json:"musicbrainz_id"`
	Hidden        bool       `json:"hidden"`
}

type ArtistVisibilityStore interface {
	GetHiddenArtists(ctx context.Context) ([]ArtistVisibility, error)
	SearchArtistsForVisibility(ctx context.Context, query string) ([]ArtistVisibility, error)
	SetArtistHidden(ctx context.Context, id int32, hidden bool) error
}
