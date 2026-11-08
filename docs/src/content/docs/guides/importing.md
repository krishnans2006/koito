---
title: Importing Data
description: How to import your existing listening history from other services into Koito.
---

Koito currently supports the following sources to import data from:

- Spotify
- Maloja
- LastFM (using https://lastfm.ghan.nl/export/)
- ListenBrainz
- YouTube Music (Google Takeout HTML)

:::note
ListenBrainz and LastFM imports can take a long time for large imports due to MusicBrainz requests being throttled at one per second. If you want these imports to go faster, you can [disable MusicBrainz](/reference/configuration/#koito_disable_musicbrainz) in the config while running the importer.
However, this means that artist aliases will not be automatically fetched for imported artists.
You can also use [your own MusicBrainz mirror](https://musicbrainz.org/doc/MusicBrainz_Server/Setup) and [disable MusicBrainz rate limiting](/reference/configuration/#koito_musicbrainz_url) in the config if you want imports to be faster.
:::

## Spotify

To get your data from Spotify, you first need to request your extended streaming history from [the Spotify privacy page](https://www.spotify.com/us/account/privacy/).
The export could take up to 30 days, according to Spotify. Then, all you have to do is put the `.json` files from your data export into the `import` folder in your config directory, and restart Koito. The data import will then start automatically.

Koito relies on file names to find files to import. If the files aren't being imported automatically, make sure they contain `Streaming_History_Audio` in the file name.

![The Spotify data export page](../../../assets/spotify_export.png)

## Maloja

You can download your data from Maloja by clicking the `Export` button under Download Data on the `/admin_overview` page of your Maloja instance.
Then, put the resuling `.json` file into the `import` folder in your config directory, and restart Koito. The data import will then start automatically.

Koito relies on file names to find files to import. If the files aren't being imported automatically, make sure they contain `maloja` in the file name.

:::note
Maloja may have missing or inconsistent track duration information, which means that the 'Hours Listened' statistic may be incorrect after a Maloja import. However, track durations will be filled in as you submit listens using the API.
:::

## LastFM

First, create an export file using [this tool from ghan.nl](https://lastfm.ghan.nl/export/) in JSON format. Then, place the resulting file into the `import` folder in your config directory.
Once you restart Koito, it will automatically detect the file as a Last FM import, and begin adding your listen activity immediately.

Koito relies on file names to find files to import. If the files aren't being imported automatically, make sure they contain `recenttracks` in the file name.

:::note
LastFM exports do not include track duration information, which means that the 'Hours Listened' statistic may be incorrect after importing.
However, track durations will be filled in as you submit listens using the API.
:::

## ListenBrainz

Create a ListenBrainz export file using [the export tool on the ListenBrainz website](https://listenbrainz.org/settings/export/). Then, place the resulting `.zip` file into the `import`
folder in your config directory. Once you restart Koito, your ListenBrainz activity will immediately start being imported.

Koito relies on file names to find files to import. If the files aren't being imported automatically, make sure they contain `listenbrainz` in the file name.

## YouTube Music

Request an export from [Google Takeout](https://takeout.google.com/), select **YouTube and YouTube Music**, include **history**, and choose **HTML** for the history format. Use an English-language export. Extract the archive and place `watch-history.html` in the `import` folder in your Koito config directory, then restart Koito. Names beginning with `watch-history` and ending with `.html` are recognized, ignoring case. JSON history files are not supported.

The importer selects YouTube Music watch activities by their activity header and `music.youtube.com` watch link. Ordinary YouTube videos are excluded, including songs watched on standard YouTube. Repeated plays and different songs at the same timestamp remain separate listens. Watch history does not include time actually played or completion status, so each eligible watch event is imported without a completion threshold. Durations represent track/video length, not measured listening time.

### Metadata and import duration

The HTML usually contains a title and channel, but no album or duration. Koito retrieves public metadata for the exact video ID from YouTube Music, with player metadata and oEmbed as fallbacks. These lookups require internet access but no Google sign-in, API key, or additional programs. Metadata requests are limited to one per second: 2,710 distinct video IDs need approximately 45 minutes of request spacing, plus fallback requests and database work. Repeated plays share cached metadata. Disabling MusicBrainz does not disable these YouTube lookups.

When metadata remains incomplete, Koito preserves the exported title and channel, removing a terminal ` - Topic` from channel names. Some channels represent uploaders rather than performing artists; these entries are identified as raw fallbacks in the report. An unavailable album uses the catalog's existing track-title-as-album fallback. Unknown duration stays at zero and can be filled by later scrobbles. Entries without a usable title or artist/channel after all lookups remain unresolved and are listed in the report.

MusicBrainz IDs are left unset during this import. Later scrobbles with matching artists, album, and track title can reuse the existing catalog entries, attach MusicBrainz IDs, and accumulate listens on the same track. Differences in titles, artist credits, or albums can prevent matching. In particular, a title-based fallback album can produce a separate track when a later scrobble supplies the real album; use the catalog's editing and merging features to reconcile these cases.

### Reports and retrying

Import logs include progress and a reconciliation summary. A report under `import_reports/` in the config directory is named using the original filename and its SHA-256 hash. It records every Music activity's original metadata, submitted metadata and its sources, missing fields, title-based album fallback, track ID, and outcome. The importer uses the existing database lookup to check each resolved track/timestamp before saving. The `submitted` count records accepted saves for keys absent at that check; `already_present` records keys found by the lookup. Import-window exclusions, invalid records, unresolved entries, and pending records are counted separately. These are submission outcomes, not atomic insert ownership: if another client writes the same track/timestamp between lookup and save, the database still keeps one listen, but the report cannot determine which client inserted it.

For example, an export with 18,954 occurrences of `music.youtube.com` can contain 18,952 actual Music records if two records repeat their URL as their displayed title. If those two records also have no recoverable title/channel, 18,950 listens are importable. The report explains these outcomes instead of treating URL occurrences as listens.

Effective metadata is saved under `youtube-music/metadata-v1/` before submitting listens. Keep this cache when retrying so imported identities remain stable. On transient network/service failures, Koito preserves the source in `import` and writes a partial report; restart Koito to retry. An import with permanently unresolved entries can still complete, with its source moved to `import_complete`. To retry unresolved entries, copy that archived HTML back to `import` and restart Koito. Previously imported listens are recognized as already present; unresolved video IDs are looked up again. Metadata already used for imported entries stays pinned, so later corrections use the catalog's editing/merging features.
