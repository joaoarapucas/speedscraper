package store

import (
	"context"
	"database/sql"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/board"
)

// BoardRef is a board with the number of verified runs stored for it.
type BoardRef struct {
	Key          board.Key
	VerifiedRuns int
	FirstFetched bool
}

// EnsureBoards creates a board for every category/level of the game that has
// at least one verified run, and returns all live boards of the game.
func (db *DB) EnsureBoards(ctx context.Context, gameID string) ([]BoardRef, error) {
	_, err := db.W.ExecContext(ctx, `INSERT OR IGNORE INTO boards (game_id, category_id, level_id)
		SELECT DISTINCT game_id, category_id, COALESCE(level_id, '') FROM runs
		WHERE game_id = ? AND status = 'verified' AND deleted_at IS NULL`, gameID)
	if err != nil {
		return nil, err
	}
	rows, err := db.W.QueryContext(ctx, `SELECT b.category_id, b.level_id, b.first_fetched_at IS NOT NULL,
			(SELECT COUNT(*) FROM runs r WHERE r.game_id = b.game_id AND r.category_id = b.category_id
				AND COALESCE(r.level_id, '') = b.level_id AND r.status = 'verified' AND r.deleted_at IS NULL)
		FROM boards b WHERE b.game_id = ? AND b.gone = 0 ORDER BY b.id`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardRef
	for rows.Next() {
		ref := BoardRef{Key: board.Key{Game: gameID}}
		if err := rows.Scan(&ref.Key.Category, &ref.Key.Level, &ref.FirstFetched, &ref.VerifiedRuns); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// BoardExists reports whether a board row exists and whether it is gone.
func (db *DB) BoardExists(ctx context.Context, k board.Key) (exists, gone bool, err error) {
	var g int
	err = db.W.QueryRowContext(ctx, "SELECT gone FROM boards WHERE game_id = ? AND category_id = ? AND level_id = ?",
		k.Game, k.Category, k.Level).Scan(&g)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	return err == nil, g == 1, err
}

// BoardResult summarizes what SaveBoard wrote.
type BoardResult struct {
	BoardID      int64
	Unchanged    bool
	Added        int
	Removed      int
	Entries      int
	RankMismatch int
}

// BoardData is a leaderboard to store: the runs fetched from the API and, for
// boards fetched in slices, stored runs ranked locally for the slices the API
// could not serve.
type BoardData struct {
	LB             *api.Leaderboard
	Local          []LocalEntry
	Split          bool
	ComputedSlices int
}

// LocalEntry is a stored run placed on a board without being fetched.
type LocalEntry struct {
	RunID    string
	Place    int
	PrimaryT float64
	Subcat   string
}

// SaveBoard stores a leaderboard fetched in a single request.
func (db *DB) SaveBoard(ctx context.Context, k board.Key, lb *api.Leaderboard, subcatKey func(*api.Run) string) (BoardResult, error) {
	return db.SaveBoardData(ctx, k, BoardData{LB: lb}, subcatKey)
}

// SaveBoardData stores a leaderboard. When its content hash matches the
// previous fetch only last_fetched_at is touched; otherwise the fetched runs
// are upserted and membership intervals are opened and closed for the runs
// that entered or left. subcatKey maps a fetched run to its subcategory
// leaderboard.
func (db *DB) SaveBoardData(ctx context.Context, k board.Key, d BoardData, subcatKey func(*api.Run) string) (BoardResult, error) {
	lb := d.LB
	entries := make([]board.Entry, 0, len(lb.Runs)+len(d.Local))
	runs := make([]api.Run, len(lb.Runs))
	byID := make(map[string]*api.Run, len(lb.Runs))
	for i := range lb.Runs {
		e := &lb.Runs[i]
		entries = append(entries, board.Entry{RunID: e.Run.ID, Place: e.Place, PrimaryT: e.Run.Times.PrimaryT})
		runs[i] = e.Run
		byID[e.Run.ID] = &runs[i]
	}
	localSubcat := make(map[string]string, len(d.Local))
	for _, e := range d.Local {
		entries = append(entries, board.Entry{RunID: e.RunID, Place: e.Place, PrimaryT: e.PrimaryT})
		localSubcat[e.RunID] = e.Subcat
	}
	subcatOf := func(id string) string {
		if r, ok := byID[id]; ok {
			return subcatKey(r)
		}
		return localSubcat[id]
	}
	hash := board.Hash(entries)
	res := BoardResult{Entries: len(entries)}
	now := db.NowString()
	split := 0
	if d.Split {
		split = 1
	}

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO boards (game_id, category_id, level_id) VALUES (?, ?, ?)",
			k.Game, k.Category, k.Level); err != nil {
			return err
		}
		var prevHash sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT id, content_hash FROM boards WHERE game_id = ? AND category_id = ? AND level_id = ?",
			k.Game, k.Category, k.Level).Scan(&res.BoardID, &prevHash); err != nil {
			return err
		}
		if prevHash.Valid && prevHash.String == hash {
			res.Unchanged = true
			_, err := tx.ExecContext(ctx, "UPDATE boards SET last_fetched_at = ?, gone = 0, split = ?, computed_slices = ? WHERE id = ?",
				now, split, d.ComputedSlices, res.BoardID)
			return err
		}

		if err := db.upsertRunsTx(ctx, tx, runs, lb.Players); err != nil {
			return err
		}
		current := map[string]struct{}{}
		rows, err := tx.QueryContext(ctx, "SELECT run_id FROM board_runs WHERE board_id = ? AND valid_to IS NULL", res.BoardID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			current[id] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		added, removed := board.Diff(current, entries)
		for _, id := range added {
			if _, err := tx.ExecContext(ctx, "INSERT INTO board_runs (board_id, run_id, subcat_key, valid_from) VALUES (?, ?, ?, ?)",
				res.BoardID, id, subcatOf(id), now); err != nil {
				return err
			}
		}
		for _, id := range removed {
			if _, err := tx.ExecContext(ctx, "UPDATE board_runs SET valid_to = ? WHERE board_id = ? AND run_id = ? AND valid_to IS NULL",
				now, res.BoardID, id); err != nil {
				return err
			}
		}
		res.Added, res.Removed = len(added), len(removed)
		res.RankMismatch = board.RankMismatches(entries)
		_, err = tx.ExecContext(ctx, `UPDATE boards SET timing = ?, content_hash = ?, run_count = ?, rank_mismatch = ?,
				first_fetched_at = COALESCE(first_fetched_at, ?), last_fetched_at = ?, last_changed_at = ?, gone = 0,
				split = ?, computed_slices = ?
			WHERE id = ?`, lb.Timing, hash, len(entries), res.RankMismatch, now, now, now, split, d.ComputedSlices, res.BoardID)
		return err
	})
	return res, err
}

// BoardSplit reports whether a board is known to be too large for the API and
// must be fetched in slices.
func (db *DB) BoardSplit(ctx context.Context, k board.Key) (bool, error) {
	var split bool
	err := db.W.QueryRowContext(ctx, "SELECT split FROM boards WHERE game_id = ? AND category_id = ? AND level_id = ?",
		k.Game, k.Category, k.Level).Scan(&split)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return split, err
}

// LocalRun is a stored verified run of a board.
type LocalRun struct {
	ID       string
	PrimaryT float64
	Date     string // "" when unknown
	Platform string
	Emulated bool
	Players  string // board.PlayersKey of "u:<user id>" and "g:<guest name>"
	Values   map[string]string
}

// BoardLocalRuns returns the stored verified, not deleted runs of a board.
func (db *DB) BoardLocalRuns(ctx context.Context, k board.Key) ([]LocalRun, error) {
	const where = `r.game_id = ? AND r.category_id = ? AND COALESCE(r.level_id, '') = ?
		AND r.status = 'verified' AND r.deleted_at IS NULL AND r.primary_t IS NOT NULL`
	args := []any{k.Game, k.Category, k.Level}
	rows, err := db.R.QueryContext(ctx, `SELECT r.id, r.primary_t, COALESCE(r.date, ''), COALESCE(r.platform_id, ''), COALESCE(r.emulated, 0)
		FROM runs r WHERE `+where+` ORDER BY r.id`, args...)
	if err != nil {
		return nil, err
	}
	var out []LocalRun
	idx := map[string]int{}
	for rows.Next() {
		lr := LocalRun{Values: map[string]string{}}
		if err := rows.Scan(&lr.ID, &lr.PrimaryT, &lr.Date, &lr.Platform, &lr.Emulated); err != nil {
			rows.Close()
			return nil, err
		}
		idx[lr.ID] = len(out)
		out = append(out, lr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.R.QueryContext(ctx, `SELECT rv.run_id, rv.variable_id, rv.value_id
		FROM run_values rv JOIN runs r ON r.id = rv.run_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, v, val string
		if err := rows.Scan(&id, &v, &val); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].Values[v] = val
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.R.QueryContext(ctx, `SELECT rp.run_id, rp.user_id, rp.guest_name
		FROM run_players rp JOIN runs r ON r.id = rp.run_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	members := map[string][]string{}
	for rows.Next() {
		var id string
		var user, guest sql.NullString
		if err := rows.Scan(&id, &user, &guest); err != nil {
			rows.Close()
			return nil, err
		}
		if user.Valid {
			members[id] = append(members[id], "u:"+user.String)
		} else {
			members[id] = append(members[id], "g:"+guest.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		m := members[out[i].ID]
		if len(m) == 0 {
			m = []string{"r:" + out[i].ID} // no players stored: never merge it with another run
		}
		out[i].Players = board.PlayersKey(m)
	}
	return out, nil
}

// MarkBoardGone closes every open membership of a board that no longer exists.
func (db *DB) MarkBoardGone(ctx context.Context, k board.Key) error {
	now := db.NowString()
	return db.Tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM boards WHERE game_id = ? AND category_id = ? AND level_id = ?",
			k.Game, k.Category, k.Level).Scan(&id)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE board_runs SET valid_to = ? WHERE board_id = ? AND valid_to IS NULL", now, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE boards SET gone = 1, content_hash = NULL, last_fetched_at = ? WHERE id = ?", now, id)
		return err
	})
}
