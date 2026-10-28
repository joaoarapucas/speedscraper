-- Timestamps are UTC text in the form 2006-01-02T15:04:05Z so that they sort
-- and compare lexicographically.

CREATE TABLE platforms  (id TEXT PRIMARY KEY, name TEXT NOT NULL, released INTEGER);
CREATE TABLE regions    (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE genres     (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE engines    (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE developers (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE publishers (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE gametypes  (id TEXT PRIMARY KEY, name TEXT NOT NULL);

CREATE TABLE series (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    name_japanese TEXT,
    abbreviation  TEXT,
    weblink       TEXT,
    created       TEXT,
    moderators    TEXT -- JSON object user_id -> role
);

CREATE TABLE games (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    name_japanese        TEXT,
    name_twitch          TEXT,
    abbreviation         TEXT,
    weblink              TEXT,
    released             INTEGER,
    release_date         TEXT,
    created              TEXT,
    romhack              INTEGER,
    show_milliseconds    INTEGER,
    require_verification INTEGER,
    require_video        INTEGER,
    run_times            TEXT, -- comma separated
    default_time         TEXT,
    emulators_allowed    INTEGER,
    raw_json             TEXT,
    discovered_at        TEXT NOT NULL,
    meta_fetched_at      TEXT,
    runs_crawled_at      TEXT,
    run_count            INTEGER,
    runs_truncated       INTEGER NOT NULL DEFAULT 0,
    gone                 INTEGER NOT NULL DEFAULT 0
);

-- Recent activity seen in the global run feeds; seeds crawl priority.
CREATE TABLE game_activity (game_id TEXT PRIMARY KEY, runs INTEGER NOT NULL);

CREATE TABLE game_platforms  (game_id TEXT NOT NULL, platform_id  TEXT NOT NULL, PRIMARY KEY (game_id, platform_id))  WITHOUT ROWID;
CREATE TABLE game_regions    (game_id TEXT NOT NULL, region_id    TEXT NOT NULL, PRIMARY KEY (game_id, region_id))    WITHOUT ROWID;
CREATE TABLE game_genres     (game_id TEXT NOT NULL, genre_id     TEXT NOT NULL, PRIMARY KEY (game_id, genre_id))     WITHOUT ROWID;
CREATE TABLE game_engines    (game_id TEXT NOT NULL, engine_id    TEXT NOT NULL, PRIMARY KEY (game_id, engine_id))    WITHOUT ROWID;
CREATE TABLE game_developers (game_id TEXT NOT NULL, developer_id TEXT NOT NULL, PRIMARY KEY (game_id, developer_id)) WITHOUT ROWID;
CREATE TABLE game_publishers (game_id TEXT NOT NULL, publisher_id TEXT NOT NULL, PRIMARY KEY (game_id, publisher_id)) WITHOUT ROWID;
CREATE TABLE game_gametypes  (game_id TEXT NOT NULL, gametype_id  TEXT NOT NULL, PRIMARY KEY (game_id, gametype_id))  WITHOUT ROWID;
CREATE TABLE game_series     (game_id TEXT NOT NULL, series_id    TEXT NOT NULL, PRIMARY KEY (game_id, series_id))    WITHOUT ROWID;
CREATE TABLE game_moderators (game_id TEXT NOT NULL, user_id      TEXT NOT NULL, role TEXT, PRIMARY KEY (game_id, user_id)) WITHOUT ROWID;

CREATE TABLE categories (
    id            TEXT PRIMARY KEY,
    game_id       TEXT NOT NULL,
    name          TEXT NOT NULL,
    weblink       TEXT,
    type          TEXT NOT NULL, -- per-game | per-level
    rules         TEXT,
    players_type  TEXT,
    players_value INTEGER,
    miscellaneous INTEGER,
    position      INTEGER,
    removed_at    TEXT
);
CREATE INDEX categories_game ON categories (game_id);

CREATE TABLE levels (
    id         TEXT PRIMARY KEY,
    game_id    TEXT NOT NULL,
    name       TEXT NOT NULL,
    weblink    TEXT,
    rules      TEXT,
    position   INTEGER,
    removed_at TEXT
);
CREATE INDEX levels_game ON levels (game_id);

CREATE TABLE variables (
    id               TEXT PRIMARY KEY,
    game_id          TEXT NOT NULL,
    name             TEXT NOT NULL,
    category_id      TEXT,
    scope_type       TEXT NOT NULL,
    scope_level_id   TEXT,
    mandatory        INTEGER,
    user_defined     INTEGER,
    obsoletes        INTEGER,
    is_subcategory   INTEGER,
    default_value_id TEXT,
    removed_at       TEXT
);
CREATE INDEX variables_game ON variables (game_id);

CREATE TABLE variable_values (
    variable_id   TEXT NOT NULL,
    id            TEXT NOT NULL,
    label         TEXT NOT NULL,
    rules         TEXT,
    miscellaneous INTEGER,
    PRIMARY KEY (variable_id, id)
) WITHOUT ROWID;

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    name_japanese TEXT,
    weblink       TEXT,
    pronouns      TEXT,
    role          TEXT,
    signup        TEXT,
    country_code  TEXT,
    country_name  TEXT,
    region_code   TEXT,
    region_name   TEXT,
    name_style    TEXT, -- JSON
    twitch        TEXT,
    hitbox        TEXT,
    youtube       TEXT,
    twitter       TEXT,
    speedrunslive TEXT,
    first_seen_at TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE guests (name TEXT PRIMARY KEY, first_seen_at TEXT NOT NULL);

CREATE TABLE runs (
    id                 TEXT PRIMARY KEY,
    game_id            TEXT NOT NULL,
    category_id        TEXT NOT NULL,
    level_id           TEXT,
    weblink            TEXT,
    status             TEXT NOT NULL, -- new | verified | rejected
    examiner_id        TEXT,
    verify_date        TEXT,
    reject_reason      TEXT,
    date               TEXT,
    date_time          TEXT,
    submitted          TEXT,
    primary_t          REAL,
    realtime_t         REAL,
    realtime_noloads_t REAL,
    ingame_t           REAL,
    platform_id        TEXT,
    emulated           INTEGER,
    region_id          TEXT,
    video_text         TEXT,
    video_uris         TEXT, -- newline separated
    comment            TEXT,
    splits_uri         TEXT,
    first_seen_at      TEXT NOT NULL,
    last_seen_at       TEXT NOT NULL,
    deleted_at         TEXT
);
CREATE INDEX runs_board ON runs (game_id, category_id, level_id);
CREATE INDEX runs_verify_date ON runs (verify_date);
CREATE INDEX runs_submitted ON runs (submitted);

CREATE TABLE run_players (
    run_id     TEXT NOT NULL,
    ord        INTEGER NOT NULL,
    user_id    TEXT,
    guest_name TEXT,
    PRIMARY KEY (run_id, ord)
) WITHOUT ROWID;
CREATE INDEX run_players_user ON run_players (user_id);

CREATE TABLE run_values (
    run_id      TEXT NOT NULL,
    variable_id TEXT NOT NULL,
    value_id    TEXT NOT NULL,
    PRIMARY KEY (run_id, variable_id)
) WITHOUT ROWID;

-- A board is one fetch of the leaderboard endpoint: a category, optionally of
-- a level. level_id is '' for full-game boards.
CREATE TABLE boards (
    id               INTEGER PRIMARY KEY,
    game_id          TEXT NOT NULL,
    category_id      TEXT NOT NULL,
    level_id         TEXT NOT NULL DEFAULT '',
    timing           TEXT,
    content_hash     TEXT,
    run_count        INTEGER,
    rank_mismatch    INTEGER NOT NULL DEFAULT 0,
    first_fetched_at TEXT,
    last_fetched_at  TEXT,
    last_changed_at  TEXT,
    gone             INTEGER NOT NULL DEFAULT 0,
    UNIQUE (game_id, category_id, level_id)
);

-- Membership history: the interval during which a run is on a board.
-- Only entries and exits are written; places are derived (see views).
CREATE TABLE board_runs (
    board_id   INTEGER NOT NULL,
    run_id     TEXT NOT NULL,
    subcat_key TEXT NOT NULL,
    valid_from TEXT NOT NULL,
    valid_to   TEXT,
    PRIMARY KEY (board_id, run_id, valid_from)
) WITHOUT ROWID;
CREATE INDEX board_runs_open ON board_runs (board_id) WHERE valid_to IS NULL;
CREATE INDEX board_runs_run ON board_runs (run_id);

CREATE VIEW board_ranking_current AS
SELECT br.board_id,
       b.game_id,
       b.category_id,
       NULLIF(b.level_id, '') AS level_id,
       br.subcat_key,
       br.run_id,
       r.primary_t,
       RANK() OVER (PARTITION BY br.board_id, br.subcat_key ORDER BY r.primary_t) AS place,
       br.valid_from
FROM board_runs br
JOIN boards b ON b.id = br.board_id
JOIN runs r ON r.id = br.run_id
WHERE br.valid_to IS NULL;

CREATE TABLE jobs (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    priority   REAL NOT NULL,
    not_before TEXT NOT NULL,
    state      TEXT NOT NULL, -- scheduled | pending | running | done | failed | gone
    rerun      INTEGER NOT NULL DEFAULT 0,
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (kind, key)
);
-- 'scheduled' jobs wait for not_before; they are promoted to 'pending' when
-- due, so every pending job is ready and the claim is a single index seek.
CREATE INDEX jobs_due ON jobs (state, not_before);
CREATE INDEX jobs_ready ON jobs (state, priority DESC, id);

CREATE TABLE checkpoints (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE alerts_sent (kind TEXT PRIMARY KEY, last_sent_at TEXT NOT NULL);
CREATE TABLE phases (name TEXT PRIMARY KEY, started_at TEXT, finished_at TEXT);
CREATE TABLE request_stats (
    minute    TEXT PRIMARY KEY,
    requests  INTEGER NOT NULL,
    throttled INTEGER NOT NULL,
    errors    INTEGER NOT NULL
);
