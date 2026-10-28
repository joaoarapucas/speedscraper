// Package board holds the pure logic behind leaderboard storage: board keys,
// subcategory resolution and membership diffs.
package board

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Key identifies a fetchable leaderboard: a category of a game, optionally
// restricted to a level. Subcategories are not part of the key because the
// unfiltered leaderboard endpoint returns the union of all of them.
type Key struct {
	Game     string
	Category string
	Level    string // "" for full-game boards
}

func (k Key) String() string { return k.Game + "|" + k.Category + "|" + k.Level }

// ParseKey is the inverse of Key.String.
func ParseKey(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return Key{}, fmt.Errorf("board: invalid key %q", s)
	}
	return Key{Game: parts[0], Category: parts[1], Level: parts[2]}, nil
}

// Var is the subset of a game variable needed to resolve subcategories.
type Var struct {
	ID            string
	CategoryID    string // "" when the variable applies to every category
	ScopeType     string // global | full-game | all-levels | single-level
	ScopeLevel    string // set when ScopeType is single-level
	IsSubcategory bool
	Default       string
}

// Subcategories returns the subcategory variables that split the board of the
// given category/level, sorted by ID.
func Subcategories(vars []Var, category, level string) []Var {
	var out []Var
	for _, v := range vars {
		if !v.IsSubcategory {
			continue
		}
		if v.CategoryID != "" && v.CategoryID != category {
			continue
		}
		if !scopeApplies(v, level) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func scopeApplies(v Var, level string) bool {
	switch v.ScopeType {
	case "global", "":
		return true
	case "full-game":
		return level == ""
	case "all-levels":
		return level != ""
	case "single-level":
		return level != "" && v.ScopeLevel == level
	}
	return false
}

// SubcatKey is the canonical "var=value;var=value" identifier of the
// subcategory leaderboard a run belongs to. subcats must come from
// Subcategories. Runs missing a value fall back to the variable default.
func SubcatKey(subcats []Var, values map[string]string) string {
	if len(subcats) == 0 {
		return ""
	}
	var b strings.Builder
	for i, v := range subcats {
		val := SubcatValue(v, values)
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(v.ID)
		b.WriteByte('=')
		b.WriteString(val)
	}
	return b.String()
}

// SubcatValue is the value a run has for a subcategory variable, falling back
// to the variable default.
func SubcatValue(v Var, values map[string]string) string {
	if val := values[v.ID]; val != "" {
		return val
	}
	return v.Default
}

// PlayersKey identifies the set of players of a run, so that runs by the same
// player or team can be compared. members are user IDs or guest names, already
// prefixed by the caller to keep the two apart.
func PlayersKey(members []string) string {
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x00")
}

// Rank returns the standard competition rank (1,2,2,4) of each time.
func Rank(times []float64) []int {
	idx := make([]int, len(times))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return times[idx[a]] < times[idx[b]] })
	places := make([]int, len(times))
	for i, j := range idx {
		if i > 0 && times[j] == times[idx[i-1]] {
			places[j] = places[idx[i-1]]
		} else {
			places[j] = i + 1
		}
	}
	return places
}

// Entry is one row of a fetched leaderboard.
type Entry struct {
	RunID    string
	Place    int
	PrimaryT float64
}

// Hash fingerprints the content of a fetched leaderboard. It is independent of
// the order in which the API lists tied runs.
func Hash(entries []Entry) string {
	rows := make([]string, len(entries))
	for i, e := range entries {
		rows[i] = fmt.Sprintf("%s:%d", e.RunID, e.Place)
	}
	sort.Strings(rows)
	h := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(h[:])
}

// Diff compares the runs currently on a board with a freshly fetched list and
// returns the runs that entered and the runs that left, both sorted.
func Diff(current map[string]struct{}, fetched []Entry) (added, removed []string) {
	seen := make(map[string]struct{}, len(fetched))
	for _, e := range fetched {
		if _, dup := seen[e.RunID]; dup {
			continue
		}
		seen[e.RunID] = struct{}{}
		if _, ok := current[e.RunID]; !ok {
			added = append(added, e.RunID)
		}
	}
	for id := range current {
		if _, ok := seen[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// RankMismatches counts entries whose official place differs from the
// standard competition rank (1,2,2,4) by primary time. A non-zero result means
// places cannot be derived from times alone for this board.
func RankMismatches(entries []Entry) int {
	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].PrimaryT < sorted[j].PrimaryT })
	n := 0
	for i, e := range sorted {
		rank := i + 1
		if i > 0 && e.PrimaryT == sorted[i-1].PrimaryT {
			rank = expectedRank(sorted, i)
		}
		if rank != e.Place {
			n++
		}
	}
	return n
}

func expectedRank(sorted []Entry, i int) int {
	for i > 0 && sorted[i].PrimaryT == sorted[i-1].PrimaryT {
		i--
	}
	return i + 1
}
