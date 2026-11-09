package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/export"
	"github.com/gabehf/koito/internal/models"
	"github.com/gabehf/koito/internal/summary"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// IDs intentionally differ from insertion/listening order and include mixed
// releases, secondary credits, aliases, and a visible artist with only hidden
// collaborations. All history is synthetic.
func visibilityFixture(t *testing.T) *Sqlite {
	t.Helper()
	s, err := NewInMemory()
	require.NoError(t, err)
	t.Cleanup(func() { s.Close(context.Background()) })
	require.NoError(t, s.Exec(`
 INSERT INTO users (id, username, role, password) VALUES (1, 'test', 'admin', x'00');
 INSERT INTO artists (id, image) VALUES (1, NULL), (2, '00000000-0000-0000-0000-000000000001'), (3, NULL), (4, NULL);
 INSERT INTO artist_aliases (artist_id, alias, source, is_primary) VALUES
 (1, 'Visible', 'Canonical', 1), (2, 'Secret', 'Canonical', 1),
 (2, 'Secret alias', 'User', 0), (3, 'Collaborator', 'Canonical', 1), (4, 'Other visible', 'Canonical', 1);
 INSERT INTO releases (id, image, various_artists) VALUES
 (10, '00000000-0000-0000-0000-000000000003', 1),
 (20, '00000000-0000-0000-0000-000000000002', 0), (30, NULL, 0), (40, NULL, 0);
 INSERT INTO release_aliases (release_id, alias, source, is_primary) VALUES
 (10, 'Mixed', 'Canonical', 1), (20, 'Hidden album', 'Canonical', 1),
 (30, 'Collaboration album', 'Canonical', 1), (40, 'Other album', 'Canonical', 1);
 INSERT INTO artist_releases (artist_id, release_id, is_primary) VALUES
 (2, 10, 1), (1, 10, 0), (2, 20, 1), (3, 30, 1), (2, 30, 0), (4, 40, 1);
 INSERT INTO tracks (id, release_id, duration) VALUES
 (100, 10, 120), (101, 10, 200), (102, 10, 180), (103, 20, 100), (104, 40, 240), (105, 30, 180);
 INSERT INTO track_aliases (track_id, alias, source, is_primary) VALUES
 (100, 'Visible song', 'Canonical', 1), (101, 'Secret song', 'Canonical', 1),
 (102, 'Duet', 'Canonical', 1), (103, 'Hidden song', 'Canonical', 1),
 (104, 'Other song', 'Canonical', 1), (105, 'Collaboration', 'Canonical', 1);
 INSERT INTO artist_tracks (artist_id, track_id, is_primary) VALUES
 (1,100,1), (2,101,1), (1,102,1), (2,102,0), (2,103,1), (4,104,1), (3,105,1), (2,105,0);
 INSERT INTO listens (track_id, listened_at, user_id) VALUES
 (101,1704067200,1), (103,1704067200,1), (105,1704067200,1), (102,1704067200,1),
 (101,1704153600,1), (100,1704240000,1), (100,1704326400,1), (104,1704412800,1);
 `))
	return s
}

func TestWebsiteVisibility(t *testing.T) {
	s := visibilityFixture(t)
	ctx := context.Background()
	web := s.WebReads()
	before, err := s.CountListens(ctx, db.Timeframe{Period: db.PeriodAllTime})
	require.NoError(t, err)
	require.EqualValues(t, 8, before)
	require.NoError(t, s.SetArtistHidden(ctx, 2, true))
	require.NoError(t, s.SetArtistHidden(ctx, 2, true)) // idempotent
	hidden, err := s.GetHiddenArtists(ctx)
	require.NoError(t, err)
	require.Len(t, hidden, 1)
	require.EqualValues(t, 2, hidden[0].ID)
	require.ErrorIs(t, s.SetArtistHidden(ctx, 9999, true), db.ErrNotFound)

	for _, item := range []struct {
		kind    string
		id      int32
		visible bool
	}{
		{"artist", 2, false}, {"artist", 3, true}, {"track", 102, false}, {"track", 105, false},
		{"album", 10, true}, {"album", 20, false}, {"album", 30, false}, {"track", 100, true},
	} {
		visible, err := web.IsVisible(ctx, item.kind, item.id)
		require.NoError(t, err)
		require.Equal(t, item.visible, visible, "%s/%d", item.kind, item.id)
	}
	_, err = web.GetArtist(ctx, db.GetArtistOpts{ID: 2})
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = web.GetTrack(ctx, db.GetTrackOpts{ID: 102})
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = web.GetAlbum(ctx, db.GetAlbumOpts{ID: 20})
	require.ErrorIs(t, err, db.ErrNotFound)
	aliases, err := web.GetAllArtistAliases(ctx, 2)
	require.NoError(t, err)
	require.Empty(t, aliases)
	aliases, err = web.GetAllTrackAliases(ctx, 102)
	require.NoError(t, err)
	require.Empty(t, aliases)
	aliases, err = web.GetAllAlbumAliases(ctx, 20)
	require.NoError(t, err)
	require.Empty(t, aliases)

	artist, err := web.GetArtist(ctx, db.GetArtistOpts{ID: 1})
	require.NoError(t, err)
	require.EqualValues(t, 2, artist.ListenCount)
	require.EqualValues(t, 240, artist.TimeListened)
	require.EqualValues(t, 1704240000, artist.FirstListen)
	require.EqualValues(t, 1, artist.AllTimeRank)
	collaborator, err := web.GetArtist(ctx, db.GetArtistOpts{ID: 3})
	require.NoError(t, err)
	require.Zero(t, collaborator.ListenCount)
	require.Zero(t, collaborator.FirstListen)
	album, err := web.GetAlbum(ctx, db.GetAlbumOpts{ID: 10})
	require.NoError(t, err)
	require.EqualValues(t, 2, album.ListenCount)
	require.EqualValues(t, 1704240000, album.FirstListen)
	require.Equal(t, []models.SimpleArtist{{ID: 1, Name: "Visible"}}, album.Artists)
	credits, err := web.GetArtistsForAlbum(ctx, 10)
	require.NoError(t, err)
	require.Len(t, credits, 1)
	require.EqualValues(t, 1, credits[0].ID)

	for _, query := range []string{"Secret", "Secret alias"} {
		artists, err := web.SearchArtists(ctx, query)
		require.NoError(t, err)
		require.Empty(t, artists)
	}
	tracks, err := web.SearchTracks(ctx, "song")
	require.NoError(t, err)
	require.Len(t, tracks, 2)
	albums, err := web.SearchAlbums(ctx, "album")
	require.NoError(t, err)
	require.Len(t, albums, 1)
	matches, err := s.SearchArtistsForVisibility(ctx, "alias")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.True(t, matches[0].Hidden)
	matches, err = s.SearchArtistsForVisibility(ctx, "%")
	require.NoError(t, err)
	require.Empty(t, matches)

	page, err := web.GetTopTracksPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 1})
	require.NoError(t, err)
	require.EqualValues(t, 2, page.TotalCount)
	require.True(t, page.HasNextPage)
	require.EqualValues(t, 100, page.Items[0].Item.ID)
	require.EqualValues(t, 1, page.Items[0].Rank)
	page, err = web.GetTopTracksPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 2, Limit: 1})
	require.NoError(t, err)
	require.False(t, page.HasNextPage)
	require.EqualValues(t, 104, page.Items[0].Item.ID)
	require.EqualValues(t, 2, page.Items[0].Rank)
	page, err = web.GetTopTracksPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20, AlbumID: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	page, err = web.GetTopTracksPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20, ArtistID: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	topArtists, err := web.GetTopArtistsPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20})
	require.NoError(t, err)
	require.Len(t, topArtists.Items, 2)
	topAlbums, err := web.GetTopAlbumsPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20})
	require.NoError(t, err)
	require.Len(t, topAlbums.Items, 2)
	topAlbums, err = web.GetTopAlbumsPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20, ArtistID: 1})
	require.NoError(t, err)
	require.Len(t, topAlbums.Items, 1)
	listens, err := web.GetListensPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 1})
	require.NoError(t, err)
	require.EqualValues(t, 3, listens.TotalCount)
	require.True(t, listens.HasNextPage)
	listens, err = web.GetListensPaginated(ctx, db.GetItemsOpts{Timeframe: db.Timeframe{Period: db.PeriodAllTime}, Page: 1, Limit: 20, ArtistID: 2})
	require.NoError(t, err)
	require.Empty(t, listens.Items)
	require.Zero(t, listens.TotalCount)

	tf := db.Timeframe{Year: 2024, Month: 1, Timezone: time.UTC}
	for _, counter := range []struct {
		name string
		f    func(context.Context, db.Timeframe) (int64, error)
		want int64
	}{
		{"listens", web.CountListens, 3}, {"seconds", web.CountTimeListened, 480},
		{"artists", web.CountArtists, 2}, {"albums", web.CountAlbums, 2}, {"tracks", web.CountTracks, 2},
		{"new artists", web.CountNewArtists, 2}, {"new albums", web.CountNewAlbums, 2}, {"new tracks", web.CountNewTracks, 2},
	} {
		actual, err := counter.f(ctx, tf)
		require.NoError(t, err, counter.name)
		require.Equal(t, counter.want, actual, counter.name)
	}
	first, err := web.GetFirstListenUnix(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1704240000, first)
	activityOpts := db.ListenActivityOpts{Year: 2024, Month: 1, Step: db.StepDay, Timezone: time.UTC}
	activity, err := web.GetListenActivity(ctx, activityOpts)
	require.NoError(t, err)
	require.Len(t, activity, 3)
	streak, err := web.GetLongestListenStreak(ctx, activityOpts)
	require.NoError(t, err)
	require.Equal(t, 3, streak)
	days, err := web.GetActiveDays(ctx, time.UTC)
	require.NoError(t, err)
	require.Equal(t, 3, days)
	interest, err := web.GetInterest(ctx, db.GetInterestOpts{ArtistID: 1, Buckets: 10})
	require.NoError(t, err)
	var total int64
	for _, bucket := range interest {
		total += bucket.ListenCount
	}
	require.EqualValues(t, 2, total)
	sum, err := summary.GenerateSummary(ctx, web, 1, tf, "")
	require.NoError(t, err)
	require.Equal(t, 3, sum.Plays)
	require.Equal(t, 2, sum.UniqueArtists)
	require.Equal(t, 2, sum.NewArtists)

	for _, image := range []struct {
		id      string
		visible bool
	}{
		{"00000000-0000-0000-0000-000000000001", false},
		{"00000000-0000-0000-0000-000000000002", false},
		{"00000000-0000-0000-0000-000000000003", true},
	} {
		visible, err := web.ImageHasAssociation(ctx, uuid.MustParse(image.id))
		require.NoError(t, err)
		require.Equal(t, image.visible, visible)
	}
	require.NoError(t, s.Exec("UPDATE artists SET image = ? WHERE id = 1", "00000000-0000-0000-0000-000000000001"))
	visible, err := web.ImageHasAssociation(ctx, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	require.NoError(t, err)
	require.True(t, visible)
	// Internal reads and writes continue to operate on the complete catalog.
	raw, err := s.GetArtist(ctx, db.GetArtistOpts{Name: "Secret alias"})
	require.NoError(t, err)
	require.EqualValues(t, 2, raw.ID)
	require.NoError(t, s.SaveListen(ctx, db.SaveListenOpts{TrackID: 101, UserID: 1, Time: time.Unix(1704499200, 0)}))
	rawCount, err := s.CountListens(ctx, tf)
	require.NoError(t, err)
	require.EqualValues(t, 9, rawCount)
	visibleCount, err := web.CountListens(ctx, tf)
	require.NoError(t, err)
	require.EqualValues(t, 3, visibleCount)
	require.NoError(t, s.SetArtistHidden(ctx, 2, false))
	visibleCount, err = web.CountListens(ctx, tf)
	require.NoError(t, err)
	require.Equal(t, rawCount, visibleCount)
}

func TestExportPreservesHiddenHistory(t *testing.T) {
	s := visibilityFixture(t)
	ctx := context.Background()

	exportListens := func() []export.KoitoListen {
		var out bytes.Buffer
		require.NoError(t, export.ExportData(ctx, &models.User{ID: 1, Username: "test"}, s, &out))
		var data export.KoitoExport
		require.NoError(t, json.Unmarshal(out.Bytes(), &data))
		return data.Listens
	}
	before := exportListens()
	require.Len(t, before, 8)
	require.NoError(t, s.SetArtistHidden(ctx, 2, true))
	after := exportListens()
	require.Equal(t, before, after)
}

func TestVisibilityMergeAndRevision(t *testing.T) {
	for _, hiddenID := range []int32{1, 2} {
		t.Run(fmt.Sprint(hiddenID), func(t *testing.T) {
			s := visibilityFixture(t)
			ctx := context.Background()
			web := s.WebReads()
			revision, err := web.WebsiteRevision(ctx)
			require.NoError(t, err)
			require.NoError(t, s.SetArtistHidden(ctx, hiddenID, true))
			updated, err := web.WebsiteRevision(ctx)
			require.NoError(t, err)
			require.NotEqual(t, revision, updated)
			require.NoError(t, s.MergeArtists(ctx, 2, 1, false))
			hidden, err := s.GetHiddenArtists(ctx)
			require.NoError(t, err)
			require.Len(t, hidden, 1)
			require.EqualValues(t, 1, hidden[0].ID)
			artist, err := s.GetArtist(ctx, db.GetArtistOpts{ID: 1})
			require.NoError(t, err)
			require.NoError(t, s.SaveArtistAliases(ctx, artist.ID, []string{"Renamed"}, "User"))
			require.NoError(t, s.SetPrimaryArtistAlias(ctx, artist.ID, "Renamed"))
			hidden, err = s.GetHiddenArtists(ctx)
			require.NoError(t, err)
			require.Equal(t, "Renamed", hidden[0].Name)
			require.NoError(t, s.AddArtistsToTrack(ctx, db.AddArtistsToTrackOpts{TrackID: 104, ArtistIDs: []int32{1}}))
			next, err := web.WebsiteRevision(ctx)
			require.NoError(t, err)
			require.NotEqual(t, updated, next)
			visible, err := web.IsVisible(ctx, "track", 104)
			require.NoError(t, err)
			require.False(t, visible)
			require.NoError(t, s.PurgeAllData(ctx))
			hidden, err = s.GetHiddenArtists(ctx)
			require.NoError(t, err)
			require.Empty(t, hidden)
			first, err := web.GetFirstListenUnix(ctx)
			require.NoError(t, err)
			require.Zero(t, first)
		})
	}
}

func TestHiddenArtistsPersistAcrossReopen(t *testing.T) {
	s := visibilityFixture(t)
	ctx := context.Background()
	require.NoError(t, s.SetArtistHidden(ctx, 2, true))
	// VACUUM INTO produces a file-backed copy including all migrations/settings.
	path := filepath.Join(t.TempDir(), "koito.db")
	require.NoError(t, s.Exec("VACUUM INTO ?", path))
	pool, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)")
	require.NoError(t, err)
	reopened := &Sqlite{db: pool}
	t.Cleanup(func() { reopened.Close(ctx) })
	hidden, err := reopened.GetHiddenArtists(ctx)
	require.NoError(t, err)
	require.Len(t, hidden, 1)
	visible, err := reopened.WebReads().IsVisible(ctx, "artist", 2)
	require.NoError(t, err)
	require.False(t, visible)
	require.NoError(t, reopened.DeleteArtist(ctx, 2))
	hidden, err = reopened.GetHiddenArtists(ctx)
	require.NoError(t, err)
	require.Empty(t, hidden)
}
