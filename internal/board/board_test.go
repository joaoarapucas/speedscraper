package board

import (
	"fmt"
	"reflect"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	for _, k := range []Key{{"g", "c", ""}, {"g", "c", "l"}} {
		got, err := ParseKey(k.String())
		if err != nil || got != k {
			t.Fatalf("%v -> %v, %v", k, got, err)
		}
	}
	for _, bad := range []string{"", "g|c", "|c|", "g||l", "a|b|c|d"} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("ParseKey(%q) should fail", bad)
		}
	}
}

func TestSubcategoriesScope(t *testing.T) {
	vars := []Var{
		{ID: "z", ScopeType: "global", IsSubcategory: true, Default: "z1"},
		{ID: "a", ScopeType: "full-game", IsSubcategory: true, Default: "a1"},
		{ID: "b", ScopeType: "all-levels", IsSubcategory: true},
		{ID: "c", ScopeType: "single-level", ScopeLevel: "L1", IsSubcategory: true},
		{ID: "d", CategoryID: "other", ScopeType: "global", IsSubcategory: true},
		{ID: "e", ScopeType: "global", IsSubcategory: false},
		{ID: "f", CategoryID: "cat", ScopeType: "global", IsSubcategory: true},
	}
	ids := func(vs []Var) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	cases := []struct {
		level string
		want  []string
	}{
		{"", []string{"a", "f", "z"}},
		{"L1", []string{"b", "c", "f", "z"}},
		{"L2", []string{"b", "f", "z"}},
	}
	for _, tc := range cases {
		if got := ids(Subcategories(vars, "cat", tc.level)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("level %q: got %v want %v", tc.level, got, tc.want)
		}
	}
}

func TestSubcatKey(t *testing.T) {
	subs := []Var{{ID: "a", Default: "a1"}, {ID: "b", Default: "b1"}}
	if got := SubcatKey(subs, map[string]string{"b": "b2", "x": "y"}); got != "a=a1;b=b2" {
		t.Fatalf("got %q", got)
	}
	if got := SubcatKey(nil, map[string]string{"b": "b2"}); got != "" {
		t.Fatalf("no subcategories should give empty key, got %q", got)
	}
}

func TestHashIgnoresTieOrder(t *testing.T) {
	a := []Entry{{"r1", 1, 10}, {"r2", 2, 11}, {"r3", 2, 11}}
	b := []Entry{{"r1", 1, 10}, {"r3", 2, 11}, {"r2", 2, 11}}
	if Hash(a) != Hash(b) {
		t.Fatal("hash depends on tie order")
	}
	c := []Entry{{"r1", 1, 10}, {"r2", 2, 11}, {"r3", 3, 12}}
	if Hash(a) == Hash(c) {
		t.Fatal("different places must change the hash")
	}
}

func TestDiff(t *testing.T) {
	set := func(ids ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	cases := []struct {
		name       string
		current    map[string]struct{}
		fetched    []Entry
		add, remov []string
	}{
		{"identical board writes nothing", set("r1", "r2"), []Entry{{RunID: "r1"}, {RunID: "r2"}}, nil, nil},
		{"new PB replaces old run of same player", set("r1", "r2", "old"), []Entry{{RunID: "r1"}, {RunID: "new"}, {RunID: "r2"}}, []string{"new"}, []string{"old"}},
		{"removed run", set("r1", "r2"), []Entry{{RunID: "r1"}}, nil, []string{"r2"}},
		{"first fetch", set(), []Entry{{RunID: "b"}, {RunID: "a"}, {RunID: "a"}}, []string{"a", "b"}, nil},
	}
	for _, tc := range cases {
		add, rem := Diff(tc.current, tc.fetched)
		if !reflect.DeepEqual(add, tc.add) || !reflect.DeepEqual(rem, tc.remov) {
			t.Errorf("%s: got +%v -%v want +%v -%v", tc.name, add, rem, tc.add, tc.remov)
		}
	}
}

func TestRankMismatches(t *testing.T) {
	ok := []Entry{{"a", 1, 10}, {"b", 2, 11}, {"c", 2, 11}, {"d", 2, 11}, {"e", 5, 12}}
	if n := RankMismatches(ok); n != 0 {
		t.Fatalf("competition ranking should match, got %d mismatches", n)
	}
	bad := []Entry{{"a", 1, 10}, {"b", 2, 11}, {"c", 3, 11}}
	if n := RankMismatches(bad); n != 1 {
		t.Fatalf("want 1 mismatch, got %d", n)
	}
}

func TestRank(t *testing.T) {
	got := Rank([]float64{30, 10, 20, 10, 40, 30})
	want := []int{4, 1, 3, 1, 6, 4}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Rank = %v, want %v", got, want)
	}
	if len(Rank(nil)) != 0 {
		t.Fatal("Rank(nil) not empty")
	}
}

func TestPlayersKey(t *testing.T) {
	if PlayersKey([]string{"u:b", "u:a"}) != PlayersKey([]string{"u:a", "u:b"}) {
		t.Fatal("order must not matter")
	}
	if PlayersKey([]string{"u:a"}) == PlayersKey([]string{"g:a"}) {
		t.Fatal("users and guests with the same name must differ")
	}
	if PlayersKey([]string{"u:ab"}) == PlayersKey([]string{"u:a", "b"}) {
		t.Fatal("separator collision")
	}
}
