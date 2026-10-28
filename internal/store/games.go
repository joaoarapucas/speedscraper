package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/board"
)

// UpsertGamesBulk records games from the bulk listing and returns the IDs that
// were not known before.
func (db *DB) UpsertGamesBulk(ctx context.Context, games []api.GameBulk) (newIDs []string, err error) {
	now := db.NowString()
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO games (id, name, name_japanese, abbreviation, weblink, discovered_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, name_japanese = excluded.name_japanese,
				abbreviation = excluded.abbreviation, weblink = excluded.weblink
			RETURNING discovered_at`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, g := range games {
			var discovered string
			if err := stmt.QueryRowContext(ctx, g.ID, g.Names.International, ptrStr(g.Names.Japanese), g.Abbreviation, g.Weblink, now).Scan(&discovered); err != nil {
				return err
			}
			if discovered == now {
				newIDs = append(newIDs, g.ID)
			}
		}
		return nil
	})
	return newIDs, err
}

// EnsureGame inserts a placeholder row for a game seen only by ID.
func (db *DB) EnsureGame(ctx context.Context, id string) (created bool, err error) {
	res, err := db.W.ExecContext(ctx, `INSERT OR IGNORE INTO games (id, name, discovered_at) VALUES (?, ?, ?)`, id, id, db.NowString())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SaveGame stores a game with its embedded metadata: dimensions, links,
// categories, levels, variables and values. Categories, levels and variables
// no longer returned by the API are flagged with removed_at.
func (db *DB) SaveGame(ctx context.Context, g *api.Game) error {
	now := db.NowString()
	return db.Tx(ctx, func(tx *sql.Tx) error {
		r := g.Ruleset
		_, err := tx.ExecContext(ctx, `INSERT INTO games (id, name, name_japanese, name_twitch, abbreviation, weblink, released,
				release_date, created, romhack, show_milliseconds, require_verification, require_video, run_times,
				default_time, emulators_allowed, raw_json, discovered_at, meta_fetched_at, gone)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, name_japanese = excluded.name_japanese,
				name_twitch = excluded.name_twitch, abbreviation = excluded.abbreviation, weblink = excluded.weblink,
				released = excluded.released, release_date = excluded.release_date, created = excluded.created,
				romhack = excluded.romhack, show_milliseconds = excluded.show_milliseconds,
				require_verification = excluded.require_verification, require_video = excluded.require_video,
				run_times = excluded.run_times, default_time = excluded.default_time,
				emulators_allowed = excluded.emulators_allowed, raw_json = excluded.raw_json,
				meta_fetched_at = excluded.meta_fetched_at, gone = 0`,
			g.ID, g.Names.International, ptrStr(g.Names.Japanese), ptrStr(g.Names.Twitch), g.Abbreviation, g.Weblink,
			g.Released, ptrStr(g.ReleaseDate), ptrStr(g.Created), boolInt(g.Romhack), boolInt(r.ShowMilliseconds),
			boolInt(r.RequireVerification), boolInt(r.RequireVideo), strings.Join(r.RunTimes, ","), r.DefaultTime,
			boolInt(r.EmulatorsAllowed), string(g.Raw), now, now)
		if err != nil {
			return err
		}

		links := []struct {
			table, col, dim string
			list            api.EmbedList[api.Resource]
		}{
			{"game_platforms", "platform_id", "platforms", g.Platforms},
			{"game_regions", "region_id", "regions", g.Regions},
			{"game_genres", "genre_id", "genres", g.Genres},
			{"game_engines", "engine_id", "engines", g.Engines},
			{"game_developers", "developer_id", "developers", g.Developers},
			{"game_publishers", "publisher_id", "publishers", g.Publishers},
			{"game_gametypes", "gametype_id", "gametypes", g.Gametypes},
		}
		for _, l := range links {
			if err := upsertResources(ctx, tx, l.dim, l.list.Items); err != nil {
				return err
			}
			if err := replaceLinks(ctx, tx, l.table, l.col, g.ID, l.list.IDs); err != nil {
				return err
			}
		}
		if err := replaceLinks(ctx, tx, "game_series", "series_id", g.ID, g.SeriesIDs()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM game_moderators WHERE game_id = ?", g.ID); err != nil {
			return err
		}
		for uid, role := range g.Moderators {
			if _, err := tx.ExecContext(ctx, "INSERT INTO game_moderators (game_id, user_id, role) VALUES (?, ?, ?)", g.ID, uid, role); err != nil {
				return err
			}
		}

		for i, c := range g.Categories.Data {
			_, err := tx.ExecContext(ctx, `INSERT INTO categories (id, game_id, name, weblink, type, rules, players_type, players_value, miscellaneous, position, removed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
				ON CONFLICT (id) DO UPDATE SET game_id = excluded.game_id, name = excluded.name, weblink = excluded.weblink,
					type = excluded.type, rules = excluded.rules, players_type = excluded.players_type,
					players_value = excluded.players_value, miscellaneous = excluded.miscellaneous,
					position = excluded.position, removed_at = NULL`,
				c.ID, g.ID, c.Name, c.Weblink, c.Type, c.Rules, c.Players.Type, c.Players.Value, boolInt(c.Miscellaneous), i)
			if err != nil {
				return err
			}
		}
		for i, l := range g.Levels.Data {
			_, err := tx.ExecContext(ctx, `INSERT INTO levels (id, game_id, name, weblink, rules, position, removed_at)
				VALUES (?, ?, ?, ?, ?, ?, NULL)
				ON CONFLICT (id) DO UPDATE SET game_id = excluded.game_id, name = excluded.name, weblink = excluded.weblink,
					rules = excluded.rules, position = excluded.position, removed_at = NULL`,
				l.ID, g.ID, l.Name, l.Weblink, l.Rules, i)
			if err != nil {
				return err
			}
		}
		for _, v := range g.Variables.Data {
			_, err := tx.ExecContext(ctx, `INSERT INTO variables (id, game_id, name, category_id, scope_type, scope_level_id, mandatory,
					user_defined, obsoletes, is_subcategory, default_value_id, removed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
				ON CONFLICT (id) DO UPDATE SET game_id = excluded.game_id, name = excluded.name,
					category_id = excluded.category_id, scope_type = excluded.scope_type,
					scope_level_id = excluded.scope_level_id, mandatory = excluded.mandatory,
					user_defined = excluded.user_defined, obsoletes = excluded.obsoletes,
					is_subcategory = excluded.is_subcategory, default_value_id = excluded.default_value_id, removed_at = NULL`,
				v.ID, g.ID, v.Name, ptrStr(v.Category), v.Scope.Type, nullStr(v.Scope.Level), boolInt(v.Mandatory),
				boolInt(v.UserDefined), boolInt(v.Obsoletes), boolInt(v.IsSubcategory), ptrStr(v.Values.Default))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM variable_values WHERE variable_id = ?", v.ID); err != nil {
				return err
			}
			for id, val := range v.Values.Values {
				var misc any
				if val.Flags != nil && val.Flags.Miscellaneous != nil {
					misc = boolInt(*val.Flags.Miscellaneous)
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO variable_values (variable_id, id, label, rules, miscellaneous) VALUES (?, ?, ?, ?, ?)",
					v.ID, id, val.Label, ptrStr(val.Rules), misc); err != nil {
					return err
				}
			}
		}
		if err := markRemoved(ctx, tx, "categories", g.ID, idsOf(g.Categories.Data, func(c api.Category) string { return c.ID }), now); err != nil {
			return err
		}
		if err := markRemoved(ctx, tx, "levels", g.ID, idsOf(g.Levels.Data, func(l api.Level) string { return l.ID }), now); err != nil {
			return err
		}
		return markRemoved(ctx, tx, "variables", g.ID, idsOf(g.Variables.Data, func(v api.Variable) string { return v.ID }), now)
	})
}

func idsOf[T any](items []T, id func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = id(it)
	}
	return out
}

func markRemoved(ctx context.Context, tx *sql.Tx, table, gameID string, keep []string, now string) error {
	keepJSON, _ := json.Marshal(keep)
	_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET removed_at = ?
		WHERE game_id = ? AND removed_at IS NULL AND id NOT IN (SELECT value FROM json_each(?))`, now, gameID, string(keepJSON))
	return err
}

func upsertResources(ctx context.Context, tx *sql.Tx, table string, items []api.Resource) error {
	for _, it := range items {
		var err error
		if table == "platforms" {
			_, err = tx.ExecContext(ctx, `INSERT INTO platforms (id, name, released) VALUES (?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET name = excluded.name, released = excluded.released`, it.ID, it.Name, it.Released)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` (id, name) VALUES (?, ?)
				ON CONFLICT (id) DO UPDATE SET name = excluded.name`, it.ID, it.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func replaceLinks(ctx context.Context, tx *sql.Tx, table, col, gameID string, ids []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE game_id = ?", gameID); err != nil {
		return err
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+" (game_id, "+col+") VALUES (?, ?)", gameID, id); err != nil {
			return err
		}
	}
	return nil
}

// MarkGameGone flags a game that the API no longer returns.
func (db *DB) MarkGameGone(ctx context.Context, id string) error {
	_, err := db.W.ExecContext(ctx, "UPDATE games SET gone = 1 WHERE id = ?", id)
	return err
}

// UpsertSeries stores series from the /series listing.
func (db *DB) UpsertSeries(ctx context.Context, series []api.Series) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		for _, s := range series {
			mods, _ := json.Marshal(s.Moderators)
			_, err := tx.ExecContext(ctx, `INSERT INTO series (id, name, name_japanese, abbreviation, weblink, created, moderators)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET name = excluded.name, name_japanese = excluded.name_japanese,
					abbreviation = excluded.abbreviation, weblink = excluded.weblink, created = excluded.created,
					moderators = excluded.moderators`,
				s.ID, s.Names.International, ptrStr(s.Names.Japanese), s.Abbreviation, s.Weblink, ptrStr(s.Created), string(mods))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// AddActivity adds recent-run counts per game.
func (db *DB) AddActivity(ctx context.Context, counts map[string]int) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		for id, n := range counts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO game_activity (game_id, runs) VALUES (?, ?)
				ON CONFLICT (game_id) DO UPDATE SET runs = runs + excluded.runs`, id, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// GameState is what the job handlers need to know about a game.
type GameState struct {
	ID          string
	Known       bool
	MetaFetched bool
	RunsCrawled bool
	RunCount    int
	Activity    int
	Gone        bool
}

func (db *DB) GameState(ctx context.Context, id string) (GameState, error) {
	st := GameState{ID: id}
	var meta, crawled sql.NullString
	var runCount sql.NullInt64
	var gone int
	err := db.W.QueryRowContext(ctx, `SELECT g.meta_fetched_at, g.runs_crawled_at, g.run_count, g.gone, COALESCE(a.runs, 0)
		FROM games g LEFT JOIN game_activity a ON a.game_id = g.id WHERE g.id = ?`, id).
		Scan(&meta, &crawled, &runCount, &gone, &st.Activity)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Known = true
	st.MetaFetched = meta.Valid
	st.RunsCrawled = crawled.Valid
	st.RunCount = int(runCount.Int64)
	st.Gone = gone == 1
	return st, nil
}

// GameIDs lists games, optionally only those not gone.
func (db *DB) GameIDs(ctx context.Context) ([]string, error) {
	rows, err := db.W.QueryContext(ctx, "SELECT id FROM games WHERE gone = 0 ORDER BY id")
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Categories returns the active categories of a game.
type CategoryInfo struct {
	ID   string
	Type string
}

func (db *DB) Categories(ctx context.Context, gameID string) ([]CategoryInfo, error) {
	rows, err := db.W.QueryContext(ctx, "SELECT id, type FROM categories WHERE game_id = ? AND removed_at IS NULL ORDER BY position", gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryInfo
	for rows.Next() {
		var c CategoryInfo
		if err := rows.Scan(&c.ID, &c.Type); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) Levels(ctx context.Context, gameID string) ([]string, error) {
	rows, err := db.W.QueryContext(ctx, "SELECT id FROM levels WHERE game_id = ? AND removed_at IS NULL ORDER BY position", gameID)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// HasCategoryAndLevel reports whether the category (and level, if set) are
// known for the game; used to detect stale metadata from the run feeds.
func (db *DB) HasCategoryAndLevel(ctx context.Context, categoryID, levelID string) (bool, error) {
	var n int
	q := "SELECT COUNT(*) FROM categories WHERE id = ? AND removed_at IS NULL"
	if err := db.W.QueryRowContext(ctx, q, categoryID).Scan(&n); err != nil || n == 0 {
		return false, err
	}
	if levelID == "" {
		return true, nil
	}
	err := db.W.QueryRowContext(ctx, "SELECT COUNT(*) FROM levels WHERE id = ? AND removed_at IS NULL", levelID).Scan(&n)
	return n > 0, err
}

// Vars returns the game's variables in the form used to compute subcategory keys.
func (db *DB) Vars(ctx context.Context, gameID string) ([]board.Var, error) {
	rows, err := db.W.QueryContext(ctx, `SELECT id, COALESCE(category_id, ''), scope_type, COALESCE(scope_level_id, ''),
			is_subcategory, COALESCE(default_value_id, '')
		FROM variables WHERE game_id = ? AND removed_at IS NULL`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []board.Var
	for rows.Next() {
		var v board.Var
		var sub int
		if err := rows.Scan(&v.ID, &v.CategoryID, &v.ScopeType, &v.ScopeLevel, &sub, &v.Default); err != nil {
			return nil, err
		}
		v.IsSubcategory = sub == 1
		out = append(out, v)
	}
	return out, rows.Err()
}
