# YouTube Music HTML history importer

## Summary and audited listen counts

Implement the feature in `internal/importer/youtube-music.go`, with startup integration, submission and duplicate accounting, focused tests, and import documentation. Use native Go HTTP lookups without requiring Python, yt-dlp, paid services, or user API credentials.

Attempt metadata enrichment first. When enrichment is incomplete, permit raw titles/channels and unknown duration. When an album is missing, leave `ReleaseTitle` empty so the existing catalog uses the track title as the album. Retain provenance and missing-field details in the import report.

Two independent read-only audits of the supplied `watch-history.html` produced:

| Measurement | Count |
|---|---:|
| Total activity records | 48,570 |
| YouTube Music watch records | **18,952** |
| Other activity records | 29,618 |
| Distinct Music video IDs | 2,710 |
| Music records containing a title and channel | 18,950 |
| Music records containing only a URL | 2 |
| Literal `music.youtube.com` occurrences | **18,954** |

The two URL-only records display their URLs as their titles, so each contributes two string occurrences. This explains the difference between 18,954 URL occurrences and 18,952 listens. Counting the text `YouTube Music` is also insufficient: one standard YouTube video mentions it in its title.

The baseline expectation on an empty database, without import-window restrictions, is **18,950 inserted listens**, subject to actual catalog matching and database duplicate accounting. The remaining video IDs, `w0mSkVskGaw` and `AybSxpvIICg`, currently yield no identifying metadata through YouTube Music's queue endpoint, player endpoint, or oEmbed. Successful recovery would increase that baseline. Explain any additional difference through per-record outcomes rather than adjusting the parser to meet an approximate count.

All Music timestamps parsed successfully and use `CST`. There are no repeated `(video ID, timestamp)` pairs, but 14 timestamps each belong to two different songs. Preserve both listens. The Music records include 18,172 Topic-channel entries and 780 other entries, including official music videos, unofficial edits, and the two URL-only records.

## Metadata strategy and research findings

The HTML supplies watch time, video URL, displayed title, and usually a channel. It has no structured album or duration metadata. The generic `Products: YouTube` caption appears on Music activity too, so use the activity header and watch-link host to identify the source.

Use YouTube Music's public metadata endpoints through a small Go HTTP client. The maintained [ytmusicapi reference](https://ytmusicapi.readthedocs.io/en/stable/reference/watch.html), [request implementation](https://github.com/sigma67/ytmusicapi/blob/main/ytmusicapi/mixins/watch.py), and [renderer parser](https://github.com/sigma67/ytmusicapi/blob/main/ytmusicapi/parsers/watch.py) establish the relevant request and response structures. These are unofficial endpoints; isolate their parsing and test renderer changes explicitly.

Live research probes established:

| Example | Result and implementation consequence |
|---|---|
| Frankie Valli's "Can't Take My Eyes off You" | Exact-ID lookup returned artist, album, and 204-second duration. |
| Passenger's anniversary acoustic recording | Returned the specific edition, album, and duration; the enriched title included a featured credit absent from the export. |
| Lexi Jayde music video | Returned a clean song title, performing artist, and duration, with no album. |
| Unofficial "no rap" edit | Returned an uploader explicitly classified as a user channel. Preserve this as raw fallback metadata. |
| Two URL-only entries | Queue/player lacked identities; oEmbed returned 404. Retain these in the unresolved report. |

Resolve metadata in this order:

1. Request YouTube Music `next` metadata for the validated video ID.
2. Select only the matching video within the actual queue, including wrapped primary/counterpart renderers. Keep metadata tied to the requested ID; a related track or a different counterpart ID is insufficient evidence for copying its album or duration.
3. Read title, explicit artist runs, album, and duration. Distinguish artist endpoints from user-channel endpoints and statistical text such as views, likes, and years.
4. Request `player` when identity or duration remains missing. Accept usable matching `videoDetails` even when playback itself is unavailable. Prefer `lengthSeconds` when available; otherwise parse the queue's duration into integer seconds.
5. Use oEmbed as a final title/channel fallback.
6. Fill remaining title/channel fields from the HTML. Leave an unknown album empty for the catalog fallback, and use duration `0` when unknown. Skip and report a record if usable title or artist/channel remains absent; a URL alone is not a track identity.

Preserve edition, remix, acoustic, live, and featured-credit text. Preserve structured artist names individually, including punctuation within names, and retain featured credits in the title even when the source omits a separate artist run. For raw channel fallback, normalize only a terminal ` - Topic`; retain other uploader names and raw video titles. Record uploader-based identities as fallbacks rather than verified performing artists. Keep Music-origin official videos and user uploads eligible.

The official [YouTube Data API](https://developers.google.com/youtube/data/reference/rest/v3/videos) exposes video/channel information and duration, but lacks structured album and performing-artist fields. Its credential requirements also conflict with the selected setup preference. oEmbed likewise provides a title and author, not a complete music identity.

Leave MusicBrainz IDs empty when importing native YouTube metadata. YouTube video/channel/browse IDs are not MusicBrainz UUIDs. Koito's existing MusicBrainz client performs known-ID lookups; speculative name searches risk associating another recording or release. Exact URL probes for the investigated IDs found no relationships. MusicBrainz's [URL lookup documentation](https://musicbrainz.org/doc/MusicBrainz_API#url_.28by_text.29) describes this alternative and its coverage limitation. General web searches can assist manual investigation of unresolved entries, but sample-specific guesses or hardcoded mappings do not belong in the importer.

## Catalog compatibility and implementation changes

### Future scrobbles with MusicBrainz IDs

Missing MusicBrainz IDs do not inherently isolate an imported track. When later metadata resolves to the same artists, album, and track title, the existing catalog can reuse the same database rows and attach MusicBrainz IDs. Historical listens remain attached to that track, so its listening history accumulates across the import and future scrobbles. Duration is not part of the matching key; zero duration can be filled later.

The relevant existing behavior is:

- `internal/catalog/associate_artists.go`: explicit name-to-MBID mappings can attach IDs to existing named artists. With an unordered list of artist MBIDs, the catalog uses MusicBrainz primary aliases to find existing artists. When MusicBrainz is unreachable, name fallback can still reuse artists, although their IDs may remain unset.
- `internal/catalog/associate_album.go`: a newly encountered release MBID can attach to an existing album without an MBID, using the resolved artist and either the supplied album title or MusicBrainz release title. The title-matching fallback also attaches a supplied release MBID when applicable.
- `internal/catalog/associate_track.go`: a newly encountered recording MBID falls back to matching the title, resolved album ID, and supplied artist IDs, then attaches the MBID to that track.
- `TestSubmitListen_AssociateAllMbzIDs` in `internal/catalog/submit_listen_test.go` already covers associating all IDs with existing entities without creating duplicates. Its unreachable-MusicBrainz counterpart covers reuse with limited artist-ID enrichment.

These matching paths assume unambiguous metadata and identities. Artist aliases and track titles use database matching rules; arbitrary differences in spelling, uploader names, editions, or artist credits are not guaranteed to reconcile. If a supplied MBID already belongs to another row, ID-first matching selects that row; it does not automatically merge a separate imported row.

The selected album fallback has a specific limitation: importing "Song A" without an album creates a title-based album "Song A". A later scrobble for "Song A" on "Album B" can create another album and track because the album identity differs. That separation results from differing metadata, not the absence of an MBID. Preserve the requested fallback, continue attempting album enrichment first, and document/report the affected entries. Automatic cross-album merging or changing catalog matching semantics is outside this feature.

### HTML parsing and submission

- Add `ImportYouTubeMusicFile(ctx, store, mbzc, filename) error`, following existing importer conventions.
- Use `golang.org/x/net/html` at v0.40.0, the version already selected by the original module graph. Promote it to a direct dependency and record its checksums; preserve all other dependency versions. Tokenize the input, scope extraction to each activity card and its primary content cell, decode HTML entities, and normalize Unicode whitespace.
- Select watch activities explicitly identified as YouTube Music with a validated `music.youtube.com/watch?v=...` link. Classify contradictory or malformed records separately. Validate the exact hostname and 11-character video ID; construct lookup URLs from that ID.
- Extract timestamp, video ID, raw title, and raw channel. Keep URL-only records eligible for enrichment. Finish parsing and checking structural integrity before submitting listens; malformed/truncated input must produce an error rather than a successful partial parse.
- Interpret the sample's `CST` explicitly as UTC-06:00, including summer dates. Support UTC/GMT, numeric offsets, and common US standard/daylight abbreviations. Report unsupported zones instead of silently treating them as UTC. Host timezone and display-timezone settings must not reinterpret the printed offset.
- Apply existing inclusive import-window rules before metadata requests. Treat each retained history event as a listen: the export has no completion or played-duration evidence, so no completion threshold or inferred-duration filtering applies.
- Keep parsing, metadata resolution, and submission separate through private entry/result types and an injectable resolver interface.
- Supply explicit artist lists to the catalog to avoid splitting verified band names through fallback parsing. Supply the mandatory `Artist` field too.
- Leave `ReleaseTitle` empty when the album is unavailable, allowing the catalog's existing track-title fallback. Record that the album was unavailable and that the stored title is a catalog fallback. Keep the existing image lookup/caching behavior and configuration unchanged.
- Submit through `catalog.SubmitListen` with `Client: "youtube-music"`, user ID `1`, and the existing import throttling/image preferences.

### Startup, public interfaces, and documentation

- Recognize case-insensitive `watch-history*.html` filenames in the configured import directory in `engine.RunImporter`. JSON history input is outside this feature.
- Preserve the existing database interfaces, SQLite implementation, and catalog APIs. An importer-local store wrapper checks each track/timestamp through `CountListensToItem` before calling the existing `SaveListen`.
- Report `submitted` for accepted writes where the key was absent at lookup and `already_present` where the key existed. These counts do not claim atomic insert ownership when another client writes the same key concurrently. Verify actual database totals independently in isolated acceptance tests.
- Document requesting the HTML export, naming requirements, metadata enrichment, catalog album fallback, raw uploader fallback, reports, expected duration, MusicBrainz compatibility, and retry procedure.
- Keep the web API, database schema, and existing catalog association policies unchanged.

## Reliability, reporting, and retries

- Bootstrap the current `WEB_REMIX` client configuration from YouTube Music using a compatible browser user agent. Isolate endpoint and renderer parsing for maintenance.
- Use context-aware requests, a 20-second timeout, an 8 MiB response limit, and a conservative one-request-per-second limiter shared by the client's metadata requests. This rate is a client policy, not an advertised service quota.
- Retry transient failures up to three total attempts with backoff and `Retry-After` handling. Exhausted transport errors, service failures, and incompatible response formats leave the import pending. Permanent unavailability or semantically missing metadata can trigger the approved raw fallback.
- Cache effective metadata by video ID in memory and under `youtube-music/metadata-v1/` in the config directory. Atomically persist each usable result before submitting its listens.
- Reuse persisted effective metadata on retries so interrupted imports retain stable identities. Keep negative unresolved results in memory only so later reimports can attempt recovery. Metadata changes requiring correction after submission use existing catalog editing/merging workflows rather than silently resubmitting history under another identity.
- Write an atomic JSON report under `import_reports/`, identified by source basename and content hash. Include source record index, video ID, original metadata, effective submitted metadata, field provenance, missing fields, fallback album behavior, outcome, and reason. Retain original timestamps and normalized timestamps.
- Reconcile every Music record into exactly one outcome: submitted, already present, outside the import window, invalid, unresolved, or pending after failure. Report resolved/partial/raw metadata as a separate breakdown to avoid double-counting.
- Keep different songs at the same timestamp. Reuse metadata for repeated video IDs while preserving every distinct listen event. Respect the database's existing `(track_id, listened_at)` duplicate rule and account for collisions after catalog association.
- Archive the HTML only after processing and report persistence succeed. Handle archival errors inside `youtube-music.go`, leaving the shared completion helper and other importers unchanged.
- Retain unresolved entries in the report and archived source. Document reimporting the archived source to retry them; stable cached identities and database duplicate handling must prevent previously imported rows from being duplicated.

At one request per distinct video ID, the sample needs approximately 45 minutes of request spacing, plus fallback requests and database work. Repeated plays reuse metadata while remaining separate listen events.

## Validation and acceptance criteria

Use synthetic HTML and sanitized HTTP fixtures for automated tests:

- Mixed Music/YouTube/YouTube TV records; unrelated "YouTube Music" text; misleading hostnames; URL-only records; escaped titles; reordered classes; Unicode spaces; truncated input.
- CST/UTC/numeric-offset conversion, unsupported zones, and inclusive import-window boundaries.
- Exact-ID selection when recommendations appear first; wrapped renderers; multiple artist runs; punctuation within band names; featured/version text; uploader classification; unavailable playback with usable metadata.
- Missing album/duration, raw fallback, and delegation of the empty album to the catalog's track-title fallback.
- HTTP failures, retry exhaustion, malformed responses, cancellation, bounded responses, and cache reuse.
- SQLite insertion counts, repeated plays, different songs at the same timestamp, interrupted-import retries, and zero new inserts on an unchanged rerun.
- Startup recognition, report persistence, successful/error archival behavior, and MusicBrainz-disabled configuration.

Add importer-specific future-scrobble regressions:

1. Import without MBIDs, then submit a later scrobble with matching title, album, artists, and MusicBrainz IDs. Assert unchanged artist/album/track IDs, persisted MBIDs, no duplicate entities, both timestamps retained, and combined listen counts.
2. Cover explicit artist-MBID mappings and unordered artist IDs with alias resolution. Cover MusicBrainz unavailability and verify name reuse without promising artist-ID attachment when alias lookup fails.
3. Import a zero-duration track, then submit a matching later scrobble with duration; verify the existing track gains duration.
4. Import with an empty album and submit a later scrobble with the same title-based album; verify reuse and ID attachment.
5. Submit a later scrobble with a different real album after a title-based fallback import; assert and document the current separate-track behavior. Cover differing raw uploader names or artist credits as metadata mismatch cases.
6. Cover a recording MBID already attached to another row and verify existing ID-first behavior; this feature must not introduce automatic merging or ID reassignment.

Run the parser audit against the supplied root file and assert 48,570 total records, 18,952 Music records, 2,710 distinct IDs, and two URL-only records. With deterministic unavailable responses for those two IDs and raw fallback elsewhere, an empty-database acceptance run must insert **18,950 listens**. A live run must explain every deviation using its per-record report.

Run the Go build and relevant importer, catalog, database, and engine tests, followed by the full suite. The research environment lacks Go and libvips, so validation so far consists of read-only HTML audits, code/test inspection, and live HTTP probes. Execute the implementation's Go checks in an environment with the project's Go 1.25 and libvips dependencies.

Keep the supplied personal history untracked and commit only small synthetic/sanitized fixtures.
