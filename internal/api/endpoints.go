package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

type envelope[T any] struct {
	Data       T           `json:"data"`
	Pagination *Pagination `json:"pagination"`
}

// Page fetches one page of a paginated collection.
func Page[T any](ctx context.Context, c *Client, path string, q url.Values, offset, max int) ([]T, error) {
	qq := cloneValues(q)
	qq.Set("max", strconv.Itoa(max))
	if offset > 0 {
		qq.Set("offset", strconv.Itoa(offset))
	}
	var env envelope[[]T]
	if err := c.Get(ctx, path, qq, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Each walks a paginated collection page by page, starting at offset, until a
// short page is returned or fn asks to stop. It returns ErrPaginationCap when
// the API refuses to go further.
func Each[T any](ctx context.Context, c *Client, path string, q url.Values, offset, max int, fn func(offset int, items []T) (stop bool, err error)) error {
	for {
		items, err := Page[T](ctx, c, path, q, offset, max)
		if err != nil {
			return err
		}
		stop, err := fn(offset, items)
		if err != nil {
			return err
		}
		if stop || len(items) < max {
			return nil
		}
		offset += max
	}
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// GameMetaEmbeds is the embed list used to fetch everything about a game in a
// single request. Moderators are not embedded so their role is preserved.
const GameMetaEmbeds = "categories,levels,variables,platforms,regions,genres,engines,developers,publishers,gametypes"

// GetGame fetches a game (by ID or abbreviation) with all metadata embedded.
func (c *Client) GetGame(ctx context.Context, idOrAbbrev string) (*Game, error) {
	var env envelope[json.RawMessage]
	q := url.Values{"embed": {GameMetaEmbeds}}
	if err := c.Get(ctx, "/games/"+url.PathEscape(idOrAbbrev), q, &env); err != nil {
		return nil, err
	}
	var g Game
	if err := json.Unmarshal(env.Data, &g); err != nil {
		return nil, fmt.Errorf("api: decode game %s: %w", idOrAbbrev, err)
	}
	g.Raw = env.Data
	return &g, nil
}

// GetLeaderboard fetches the full leaderboard of a category, or of a level
// when levelID is not empty. Without var- filters the API returns the union of
// every subcategory leaderboard of that category/level. filter adds query
// parameters such as var-{id}, platform or emulators.
func (c *Client) GetLeaderboard(ctx context.Context, gameID, categoryID, levelID string, filter url.Values) (*Leaderboard, error) {
	path := "/leaderboards/" + url.PathEscape(gameID) + "/category/" + url.PathEscape(categoryID)
	if levelID != "" {
		path = "/leaderboards/" + url.PathEscape(gameID) + "/level/" + url.PathEscape(levelID) + "/" + url.PathEscape(categoryID)
	}
	q := cloneValues(filter)
	q.Set("embed", "players")
	var env envelope[Leaderboard]
	if err := c.Get(ctx, path, q, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}
