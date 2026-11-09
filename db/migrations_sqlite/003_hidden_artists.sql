-- +goose Up
CREATE TABLE hidden_artists (
    artist_id INTEGER PRIMARY KEY REFERENCES artists(id) ON DELETE CASCADE
);

CREATE TABLE website_revision (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL DEFAULT 0
);
INSERT INTO website_revision (id) VALUES (1);

-- +goose StatementBegin
CREATE TRIGGER website_revision_hidden_artists_insert
AFTER INSERT ON hidden_artists
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_hidden_artists_delete
AFTER DELETE ON hidden_artists
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_artist_tracks_insert
AFTER INSERT ON artist_tracks
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_artist_tracks_update
AFTER UPDATE ON artist_tracks
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_artist_tracks_delete
AFTER DELETE ON artist_tracks
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_tracks_update
AFTER UPDATE ON tracks
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_tracks_delete
AFTER DELETE ON tracks
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_listens_insert
AFTER INSERT ON listens
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_listens_update
AFTER UPDATE ON listens
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER website_revision_listens_delete
AFTER DELETE ON listens
BEGIN
    UPDATE website_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER website_revision_hidden_artists_insert;
DROP TRIGGER website_revision_hidden_artists_delete;
DROP TRIGGER website_revision_artist_tracks_insert;
DROP TRIGGER website_revision_artist_tracks_update;
DROP TRIGGER website_revision_artist_tracks_delete;
DROP TRIGGER website_revision_tracks_update;
DROP TRIGGER website_revision_tracks_delete;
DROP TRIGGER website_revision_listens_insert;
DROP TRIGGER website_revision_listens_update;
DROP TRIGGER website_revision_listens_delete;
DROP TABLE website_revision;
DROP TABLE hidden_artists;
