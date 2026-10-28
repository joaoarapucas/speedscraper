package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/joaoarapucas/speedscraper/internal/api"
)

type runStmts struct {
	run, delPlayers, player, delValues, value, user, guest *sql.Stmt
}

func prepareRunStmts(ctx context.Context, tx *sql.Tx) (*runStmts, func(), error) {
	s := &runStmts{}
	defs := []struct {
		dst **sql.Stmt
		q   string
	}{
		{&s.run, `INSERT INTO runs (id, game_id, category_id, level_id, weblink, status, examiner_id, verify_date, reject_reason,
				date, date_time, submitted, primary_t, realtime_t, realtime_noloads_t, ingame_t, platform_id, emulated, region_id,
				video_text, video_uris, comment, splits_uri, first_seen_at, last_seen_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT (id) DO UPDATE SET game_id = excluded.game_id, category_id = excluded.category_id,
				level_id = excluded.level_id, weblink = excluded.weblink, status = excluded.status,
				examiner_id = excluded.examiner_id, verify_date = excluded.verify_date,
				reject_reason = excluded.reject_reason, date = excluded.date, date_time = excluded.date_time,
				submitted = excluded.submitted, primary_t = excluded.primary_t, realtime_t = excluded.realtime_t,
				realtime_noloads_t = excluded.realtime_noloads_t, ingame_t = excluded.ingame_t,
				platform_id = excluded.platform_id, emulated = excluded.emulated, region_id = excluded.region_id,
				video_text = excluded.video_text, video_uris = excluded.video_uris, comment = excluded.comment,
				splits_uri = excluded.splits_uri, last_seen_at = excluded.last_seen_at, deleted_at = NULL`},
		{&s.delPlayers, "DELETE FROM run_players WHERE run_id = ?"},
		{&s.player, "INSERT INTO run_players (run_id, ord, user_id, guest_name) VALUES (?, ?, ?, ?)"},
		{&s.delValues, "DELETE FROM run_values WHERE run_id = ?"},
		{&s.value, "INSERT INTO run_values (run_id, variable_id, value_id) VALUES (?, ?, ?)"},
		{&s.user, `INSERT INTO users (id, name, name_japanese, weblink, pronouns, role, signup, country_code, country_name,
				region_code, region_name, name_style, twitch, hitbox, youtube, twitter, speedrunslive, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, name_japanese = excluded.name_japanese,
				weblink = excluded.weblink, pronouns = excluded.pronouns, role = excluded.role, signup = excluded.signup,
				country_code = excluded.country_code, country_name = excluded.country_name,
				region_code = excluded.region_code, region_name = excluded.region_name, name_style = excluded.name_style,
				twitch = excluded.twitch, hitbox = excluded.hitbox, youtube = excluded.youtube, twitter = excluded.twitter,
				speedrunslive = excluded.speedrunslive, updated_at = excluded.updated_at`},
		{&s.guest, "INSERT OR IGNORE INTO guests (name, first_seen_at) VALUES (?, ?)"},
	}
	var opened []*sql.Stmt
	closeAll := func() {
		for _, st := range opened {
			st.Close()
		}
	}
	for _, d := range defs {
		st, err := tx.PrepareContext(ctx, d.q)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		*d.dst = st
		opened = append(opened, st)
	}
	return s, closeAll, nil
}

// UpsertRuns stores runs with their players and variable values, plus any
// users embedded in the runs or passed in players (leaderboard embeds).
func (db *DB) UpsertRuns(ctx context.Context, runs []api.Run, players api.Players) error {
	return db.Tx(ctx, func(tx *sql.Tx) error { return db.upsertRunsTx(ctx, tx, runs, players) })
}

func (db *DB) upsertRunsTx(ctx context.Context, tx *sql.Tx, runs []api.Run, players api.Players) error {
	now := db.NowString()
	s, closeAll, err := prepareRunStmts(ctx, tx)
	if err != nil {
		return err
	}
	defer closeAll()
	for _, p := range players {
		if err := s.upsertPlayer(ctx, p, now); err != nil {
			return err
		}
	}
	for i := range runs {
		r := &runs[i]
		var videoText, videoURIs any
		if r.Videos != nil {
			videoText = ptrStr(r.Videos.Text)
			uris := make([]string, 0, len(r.Videos.Links))
			for _, l := range r.Videos.Links {
				uris = append(uris, l.URI)
			}
			videoURIs = nullStr(strings.Join(uris, "\n"))
		}
		var splits any
		if r.Splits != nil {
			splits = nullStr(r.Splits.URI)
		}
		_, err := s.run.ExecContext(ctx, r.ID, r.Game, r.Category, ptrStr(r.Level), r.Weblink, r.Status.Status,
			ptrStr(r.Status.Examiner), ptrStr(r.Status.VerifyDate), ptrStr(r.Status.Reason), ptrStr(r.Date),
			ptrStr(r.DateTime), ptrStr(r.Submitted), r.Times.PrimaryT, r.Times.RealtimeT, r.Times.RealtimeNoloadsT,
			r.Times.IngameT, ptrStr(r.System.Platform), boolInt(r.System.Emulated), ptrStr(r.System.Region),
			videoText, videoURIs, ptrStr(r.Comment), splits, now, now)
		if err != nil {
			return err
		}
		if _, err := s.delPlayers.ExecContext(ctx, r.ID); err != nil {
			return err
		}
		for ord, p := range r.Players {
			var uid, guest any
			if p.Rel == "user" {
				uid = p.ID
			} else {
				guest = p.Name
			}
			if _, err := s.player.ExecContext(ctx, r.ID, ord, uid, guest); err != nil {
				return err
			}
			if err := s.upsertPlayer(ctx, p, now); err != nil {
				return err
			}
		}
		if _, err := s.delValues.ExecContext(ctx, r.ID); err != nil {
			return err
		}
		for vid, val := range r.Values {
			if _, err := s.value.ExecContext(ctx, r.ID, vid, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *runStmts) upsertPlayer(ctx context.Context, p api.Player, now string) error {
	switch {
	case p.Rel == "guest" && p.Name != "":
		_, err := s.guest.ExecContext(ctx, p.Name, now)
		return err
	case p.User != nil:
		u := p.User
		var cc, cn, rc, rn any
		if u.Location != nil {
			if u.Location.Country != nil {
				cc, cn = u.Location.Country.Code, u.Location.Country.Names.International
			}
			if u.Location.Region != nil {
				rc, rn = u.Location.Region.Code, u.Location.Region.Names.International
			}
		}
		uri := func(r *api.URIRef) any {
			if r == nil {
				return nil
			}
			return r.URI
		}
		var style any
		if len(u.NameStyle) > 0 {
			style = string(u.NameStyle)
		}
		_, err := s.user.ExecContext(ctx, u.ID, u.Names.International, ptrStr(u.Names.Japanese), u.Weblink,
			ptrStr(u.Pronouns), u.Role, ptrStr(u.Signup), cc, cn, rc, rn, style, uri(u.Twitch), uri(u.Hitbox),
			uri(u.Youtube), uri(u.Twitter), uri(u.SpeedRunsLive), now, now)
		return err
	}
	return nil
}

// FinishRunsCrawl records the exact run count of a game after a complete
// crawl that started at crawlStart and flags runs not seen since then as
// deleted (only when the crawl was not truncated by the pagination cap).
func (db *DB) FinishRunsCrawl(ctx context.Context, gameID, crawlStart string, truncated bool) (runCount int, err error) {
	now := db.NowString()
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		if !truncated {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET deleted_at = ?
				WHERE game_id = ? AND last_seen_at < ? AND deleted_at IS NULL`, now, gameID, crawlStart); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE game_id = ? AND deleted_at IS NULL", gameID).Scan(&runCount); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE games SET runs_crawled_at = ?, run_count = ?, runs_truncated = ? WHERE id = ?",
			now, runCount, boolInt(truncated), gameID)
		return err
	})
	return runCount, err
}

// RunSnapshot is the stored state of a run that matters for leaderboards.
type RunSnapshot struct {
	Status   string
	PrimaryT float64
}

// RunSnapshots returns the stored snapshot of the given runs (absent runs are
// missing from the map).
func (db *DB) RunSnapshots(ctx context.Context, ids []string) (map[string]RunSnapshot, error) {
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := db.W.QueryContext(ctx, `SELECT id, status, COALESCE(primary_t, 0) FROM runs
		WHERE id IN (SELECT value FROM json_each(?)) AND deleted_at IS NULL`, string(idsJSON))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]RunSnapshot, len(ids))
	for rows.Next() {
		var id string
		var s RunSnapshot
		if err := rows.Scan(&id, &s.Status, &s.PrimaryT); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}
