# Hidden artists: feasibility and implementation plan

## Feasibility

Feasible with the existing Go/SQLite backend and React client. This is a moderately sized change across website queries, rather than just a settings control. No scrobbles or catalog associations need to be deleted or rewritten.

Relevant existing structure:

- Settings live in `client/app/components/modals/SettingsModal.tsx`; authenticated users currently see account/API/export controls.
- Artists have stable local IDs, MusicBrainz IDs, and separate aliases. Track and release credits use `artist_tracks` and `artist_releases`.
- `internal/db/sqlite/artist.go`, `album.go`, `track.go`, `listen.go`, `counts.go`, `listen_activity.go`, and `interest.go` contain independent read queries. Many are shared with catalog ingestion.
- Website routes are bound in `engine/routes.go`. Authentication exists, and `models.User.Role` distinguishes admins, but authentication middleware does not itself enforce an admin role.
- Export already uses a separate `ExportStore`, `GetExportPage`, and `artistsWithAliasesForTrack`. This separation should be preserved.
- Statistics are cached for 30 minutes in `engine/handlers/stats.go`. The client uses React Query, plus separately fetched first-activity state in `AppProvider`.

The principal risks are missed read paths, stale cached content, incorrect pagination/rank calculations, and accidentally hiding records from ingestion lookups. These are manageable with a single visibility policy and integration tests.

## Proposed behavior

This plan assumes that “completely hidden” means excluding the artist's tracks and listens from website views and displayed statistics. This is a proposed product decision, pending the user's preference; hiding only artist identities would be a smaller alternative.

- The hidden list is instance-wide and applies to visitors and logged-in users, including admins browsing normal pages.
- The admin-only hidden-artists settings control is the intentional exception: it must display the selected artists so they can be unhidden.
- Hidden artists disappear from pages, search, credits, charts, summaries/Rewind, recent listens, and now playing.
- A track credited to any hidden artist is hidden, including collaborations and non-primary credits. Its scrobbles remain stored but do not contribute to website statistics, heatmaps, streaks, rankings, or first-listen dates.
- An album with visible tracks remains available, showing only visible tracks and non-hidden artist credits. An album with no visible tracks disappears. This preserves unrelated songs on compilations rather than hiding an entire release because of one contributor.
- Artist, album, and track detail statistics are recomputed from visible listens. Discovery/new-item dates use the first visible listen, not an earlier hidden listen. A non-hidden artist remains directly addressable even if all of their scrobbles were hidden collaborations; their displayed activity is empty.
- Direct detail, aliases, credits, and interest URLs for hidden entities return 404. Collection queries scoped to a hidden entity must not disclose its data or counts.
- Imports, new scrobbles, metadata enrichment, and ListenBrainz relay continue normally. Future scrobbles associated with the same hidden artist ID are automatically hidden.
- Authenticated exports retain every scrobble and its original full artist metadata. Unhiding restores website visibility without reimporting.
- Initial scope selects artists already in the database. Name patterns, regexes, and rules for artists not yet imported are separate features. Aliases/renames of an existing artist remain covered by its ID.
- Filtering follows catalog relationships, rather than attempting to recognize artist names in arbitrary track titles or other free text.

## 1. Persist visibility and expose admin controls

Add migration `003_hidden_artists.sql` with a `hidden_artists` table whose `artist_id` is a primary key and foreign key to `artists(id)` with `ON DELETE CASCADE`. Start empty. Do not alter existing named catalog views to exclude hidden records.

Add a dedicated `ArtistVisibilityStore` interface and SQLite implementation for listing, adding, and removing hidden artist IDs. Validate IDs and make writes idempotent and transactional.

Proposed routes under `/apis/web/v1/admin`:

- `GET /hidden-artists`: list current entries with ID, current primary name, and optional MBID.
- `PUT /hidden-artists/{id}`: hide an existing artist.
- `DELETE /hidden-artists/{id}`: unhide an artist.
- `GET /artists/search?q=...`: artist-only catalog search, including hidden entries and their hidden status, used exclusively by settings.

Use existing session authentication plus an explicit admin-role check. Reject anonymous access with 401 and non-admin access with 403, including reads and search. Do not add a public `include_hidden` query flag or expose the hidden list through `/config`.

Maintain visibility during catalog operations: merging either a hidden source or hidden destination produces a hidden destination, within the merge transaction. Explicit artist deletion removes its hidden entry through the foreign key. Data purge clears entries along with the catalog. Rename, alias, and MBID changes preserve the local-ID association.

## 2. Introduce a website-only read path

Introduce a narrow `WebReadStore` adapter exposing the reads needed by website handlers. Keep raw catalog reads, writes, background jobs, importer duplicate checks, and `ExportStore` unchanged. Refactor shared SQL implementations to accept an internal visibility scope where needed; callers cannot set this scope through HTTP parameters.

Centralize the relational policy, using UI-specific SQL views or reusable CTEs:

- visible artists: no matching `hidden_artists` entry;
- visible tracks: no hidden artist in `artist_tracks`;
- visible listens: listens whose track is visible;
- visible albums: releases with at least one visible track;
- visible credits: exclude hidden artists from track/release credit lists.

Use `NOT EXISTS` for exclusion so collaboration joins do not multiply counts. Preserve existing indexes; inspect query plans on a realistic database before adding further indexes.

Wire every ordinary website read through this adapter, including reads behind edit dialogs. Keep admin hidden-list management and exports explicitly on their authorized unfiltered paths. A narrow read interface should prevent an accidentally inherited raw read method from bypassing filtering.

Apply visibility before SQL aggregation, ranking windows, search candidate limits, counts, and pagination. Filtering serialized responses after fetching a page would leave holes, wrong totals, and ranks influenced by hidden listens.

## 3. Cover all website surfaces

Audit the routes in `engine/routes.go` against this checklist:

- Artist/album/track detail, aliases, credits, and interest graphs.
- All three top lists, their artist/album-scoped variants, total counts, ranks, and detail all-time ranks.
- Listen history, scoped history, first activity, active days, streaks, activity grids, and all displayed counts/durations/ratios.
- Search results for artists, albums, and tracks, including aliases and nested credits.
- Summary generation in `internal/summary`, used by summary/Rewind views.
- Now playing: check the cached track ID against current visibility on every read; respond with `currently_playing: false` for a hidden track. Do not change ingestion's now-playing bookkeeping.

Handle empty histories explicitly: zero-valued statistics, no division by zero, empty charts, and a defined no-first-activity response understood by the client.

Image URLs are a separate path: `ImageHandler` currently serves disk-cache hits before checking database associations, with a 30-day public cache lifetime. To suppress direct origin access as well as removing UI references, check for a visible artist/release association before serving cached files. Shared artwork remains accessible if a visible entity uses it. Retain image files and raw image-source lookups for exports/background work. Previously downloaded or browser-cached images cannot be recalled; the feature controls future website responses, not previously obtained copies.

## 4. Refresh cached views

After visibility changes, prevent reuse of old statistics. Use a visibility revision in website cache keys, or equivalent synchronized invalidation, so an in-flight old request cannot repopulate the current cache with stale totals. Apply the same rule when merges or credit edits change effective visibility. Do not clear unrelated `memkv` data such as now-playing state.

In the saving browser, cancel/remove affected React Query data and refetch it; also refresh first-activity state and route-loaded detail/search data. If the current detail page becomes hidden, transition to the normal unavailable-page state. Clear admin hidden-search/list query data on logout.

Other open browsers refresh through their normal requests unless cross-browser push updates are added. Define the guarantee as filtered responses after a successful save; already rendered pages require a refresh. Do not make live push infrastructure a prerequisite for this feature.

## 5. Build the settings UI

Add an admin-only “Hidden artists” tab to `SettingsModal.tsx`, a dedicated component, and typed API functions in `client/api/api.ts`.

Provide debounced artist search across names/aliases, a list of hidden artists, and Add/Unhide actions. Show MBIDs or local IDs to disambiguate artists with the same name. Support repeated additions so the administrator can build a list without ambiguous free-text matching. Show loading, empty, save-error, and success states; only report success after the server commits.

Explain the behavior in plain text: “Hidden artists and their tracks are excluded from website views and statistics. Their scrobbles remain stored and included in exports.”

## 6. Validate before release

Use synthetic fixtures with one visible artist, one hidden artist, a shared-credit track, a mixed compilation, aliases, and enough rows for multiple pages.

- Migration starts with every artist visible and persists selections across reopen/restart.
- Every website route above excludes hidden data for both anonymous and authenticated browsing. Admin-only list/search endpoints enforce roles.
- Collaborations hide the entire track; unrelated compilation tracks remain visible; nested credits contain no hidden artist.
- Pagination totals, ranks, timeframe filters, first-visible dates, discovery counts, activity, and statistics agree with the visible listen set.
- Hide/unhide takes effect with warm caches, cached now playing, an open detail page, and in-flight queries. Entirely hidden histories render correctly.
- New submissions and imports reuse the existing hidden artist/track IDs and retain all listens; hiding itself never changes raw catalog/listen counts.
- Renames, alias changes, both merge directions, and credit changes preserve the chosen behavior.
- Exports before and after hiding have identical listen and metadata content, excluding generated export timestamps. Exercise multiple export pages and same-timestamp tracks. Reimported scrobbles remain intact; the hidden list is instance configuration and is not added to export v1 in this scope.
- Cached and uncached direct image requests obey the visible-association policy.

Run targeted SQLite/handler/catalog/export/summary tests, then `go test ./... -timeout 60s` and the API build with the project's Go/libvips prerequisites. Run `yarn typecheck` and `yarn build` in `client`, followed by browser checks of settings, home, search, charts, details, Rewind, and export. Compare query plans and timings with no hidden artists and with a representative list on a large history.

## Delivery order

1. Migration, visibility store, role checks, merge semantics, and admin API.
2. Filtered website read adapter and complete route/query coverage.
3. Cache handling and settings UI.
4. Export/ingestion regressions, browser validation, and user documentation.

Ship these together: exposing a hide control before all read paths are covered would create inconsistent behavior. No feature implementation or runtime changes are included in this planning document.
