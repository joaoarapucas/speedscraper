package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Pagination is the pagination element attached to paginated collections.
type Pagination struct {
	Offset int    `json:"offset"`
	Max    int    `json:"max"`
	Size   int    `json:"size"`
	Links  []Link `json:"links"`
}

type Link struct {
	Rel string `json:"rel"`
	URI string `json:"uri"`
}

type Names struct {
	International string  `json:"international"`
	Japanese      *string `json:"japanese"`
	Twitch        *string `json:"twitch"`
}

// Resource covers the simple resources (platform, region, genre, engine,
// developer, publisher, gametype). Released is only set for platforms.
type Resource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Released *int   `json:"released"`
}

// EmbedList decodes a field that is either a list of IDs (not embedded) or an
// embedded {"data": [...]} object. IDs always holds the IDs; Items is only
// filled when the field was embedded.
type EmbedList[T any] struct {
	IDs   []string
	Items []T
}

func (e *EmbedList[T]) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if b[0] == '[' {
		return json.Unmarshal(b, &e.IDs)
	}
	var wrapped struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return err
	}
	for _, raw := range wrapped.Data {
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		var id struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &id)
		e.Items = append(e.Items, item)
		e.IDs = append(e.IDs, id.ID)
	}
	return nil
}

// Embedded decodes an embedded {"data": [...]} collection.
type Embedded[T any] struct {
	Data []T `json:"data"`
}

type Ruleset struct {
	ShowMilliseconds    bool     `json:"show-milliseconds"`
	RequireVerification bool     `json:"require-verification"`
	RequireVideo        bool     `json:"require-video"`
	RunTimes            []string `json:"run-times"`
	DefaultTime         string   `json:"default-time"`
	EmulatorsAllowed    bool     `json:"emulators-allowed"`
}

// GameBulk is the reduced game representation returned in bulk mode.
type GameBulk struct {
	ID           string `json:"id"`
	Names        Names  `json:"names"`
	Abbreviation string `json:"abbreviation"`
	Weblink      string `json:"weblink"`
}

type Game struct {
	ID           string              `json:"id"`
	Names        Names               `json:"names"`
	Abbreviation string              `json:"abbreviation"`
	Weblink      string              `json:"weblink"`
	Released     *int                `json:"released"`
	ReleaseDate  *string             `json:"release-date"`
	Ruleset      Ruleset             `json:"ruleset"`
	Romhack      bool                `json:"romhack"`
	Gametypes    EmbedList[Resource] `json:"gametypes"`
	Platforms    EmbedList[Resource] `json:"platforms"`
	Regions      EmbedList[Resource] `json:"regions"`
	Genres       EmbedList[Resource] `json:"genres"`
	Engines      EmbedList[Resource] `json:"engines"`
	Developers   EmbedList[Resource] `json:"developers"`
	Publishers   EmbedList[Resource] `json:"publishers"`
	Moderators   map[string]string   `json:"moderators"`
	Created      *string             `json:"created"`
	Links        []Link              `json:"links"`
	Categories   Embedded[Category]  `json:"categories"`
	Levels       Embedded[Level]     `json:"levels"`
	Variables    Embedded[Variable]  `json:"variables"`
	Raw          json.RawMessage     `json:"-"`
}

// SeriesIDs extracts the series the game belongs to from its links.
func (g *Game) SeriesIDs() []string {
	var ids []string
	for _, l := range g.Links {
		if l.Rel == "series" {
			ids = append(ids, lastPathSegment(l.URI))
		}
	}
	return ids
}

func lastPathSegment(uri string) string {
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '/' {
			return uri[i+1:]
		}
	}
	return uri
}

type Series struct {
	ID           string            `json:"id"`
	Names        Names             `json:"names"`
	Abbreviation string            `json:"abbreviation"`
	Weblink      string            `json:"weblink"`
	Moderators   map[string]string `json:"moderators"`
	Created      *string           `json:"created"`
}

type Category struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Weblink string `json:"weblink"`
	Type    string `json:"type"`
	Rules   string `json:"rules"`
	Players struct {
		Type  string `json:"type"`
		Value int    `json:"value"`
	} `json:"players"`
	Miscellaneous bool `json:"miscellaneous"`
}

type Level struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Weblink string `json:"weblink"`
	Rules   string `json:"rules"`
}

type VariableValue struct {
	Label string  `json:"label"`
	Rules *string `json:"rules"`
	Flags *struct {
		Miscellaneous *bool `json:"miscellaneous"`
	} `json:"flags"`
}

type Variable struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category *string `json:"category"`
	Scope    struct {
		Type  string `json:"type"`
		Level string `json:"level"`
	} `json:"scope"`
	Mandatory     bool `json:"mandatory"`
	UserDefined   bool `json:"user-defined"`
	Obsoletes     bool `json:"obsoletes"`
	IsSubcategory bool `json:"is-subcategory"`
	Values        struct {
		Values  map[string]VariableValue `json:"values"`
		Default *string                  `json:"default"`
	} `json:"values"`
}

type URIRef struct {
	URI string `json:"uri"`
}

type Color struct {
	Light string `json:"light"`
	Dark  string `json:"dark"`
}

type Location struct {
	Country *struct {
		Code  string `json:"code"`
		Names Names  `json:"names"`
	} `json:"country"`
	Region *struct {
		Code  string `json:"code"`
		Names Names  `json:"names"`
	} `json:"region"`
}

type User struct {
	ID            string          `json:"id"`
	Names         Names           `json:"names"`
	Weblink       string          `json:"weblink"`
	Pronouns      *string         `json:"pronouns"`
	NameStyle     json.RawMessage `json:"name-style"`
	Role          string          `json:"role"`
	Signup        *string         `json:"signup"`
	Location      *Location       `json:"location"`
	Twitch        *URIRef         `json:"twitch"`
	Hitbox        *URIRef         `json:"hitbox"`
	Youtube       *URIRef         `json:"youtube"`
	Twitter       *URIRef         `json:"twitter"`
	SpeedRunsLive *URIRef         `json:"speedrunslive"`
}

// Player is one participant of a run. For rel "user" ID is set, for rel
// "guest" Name is set. User is filled only when players were embedded.
type Player struct {
	Rel  string
	ID   string
	Name string
	User *User
}

// Players decodes both the plain reference list and the embedded
// {"data": [...]} form of a run's players.
type Players []Player

func (p *Players) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	var raws []json.RawMessage
	if b[0] == '[' {
		if err := json.Unmarshal(b, &raws); err != nil {
			return err
		}
	} else {
		var wrapped struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(b, &wrapped); err != nil {
			return err
		}
		raws = wrapped.Data
	}
	out := make(Players, 0, len(raws))
	for _, raw := range raws {
		pl, err := decodePlayer(raw)
		if err != nil {
			return err
		}
		out = append(out, pl)
	}
	*p = out
	return nil
}

func decodePlayer(raw json.RawMessage) (Player, error) {
	var head struct {
		Rel   string          `json:"rel"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Names json.RawMessage `json:"names"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Player{}, err
	}
	pl := Player{Rel: head.Rel, ID: head.ID, Name: head.Name}
	switch {
	case head.Rel == "user" && head.Names != nil:
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return Player{}, err
		}
		pl.User = &u
	case head.Rel == "user", head.Rel == "guest":
	default:
		return Player{}, fmt.Errorf("unknown player rel %q", head.Rel)
	}
	return pl, nil
}

type RunStatus struct {
	Status     string  `json:"status"`
	Examiner   *string `json:"examiner"`
	VerifyDate *string `json:"verify-date"`
	Reason     *string `json:"reason"`
}

type RunTimes struct {
	PrimaryT         float64 `json:"primary_t"`
	RealtimeT        float64 `json:"realtime_t"`
	RealtimeNoloadsT float64 `json:"realtime_noloads_t"`
	IngameT          float64 `json:"ingame_t"`
}

type Run struct {
	ID       string  `json:"id"`
	Weblink  string  `json:"weblink"`
	Game     string  `json:"game"`
	Level    *string `json:"level"`
	Category string  `json:"category"`
	Videos   *struct {
		Text  *string  `json:"text"`
		Links []URIRef `json:"links"`
	} `json:"videos"`
	Comment   *string   `json:"comment"`
	Status    RunStatus `json:"status"`
	Players   Players   `json:"players"`
	Date      *string   `json:"date"`
	DateTime  *string   `json:"date-time"`
	Submitted *string   `json:"submitted"`
	Times     RunTimes  `json:"times"`
	System    struct {
		Platform *string `json:"platform"`
		Emulated bool    `json:"emulated"`
		Region   *string `json:"region"`
	} `json:"system"`
	Splits *struct {
		URI string `json:"uri"`
	} `json:"splits"`
	Values map[string]string `json:"values"`
}

// LevelID returns the run's level or "" for full-game runs.
func (r *Run) LevelID() string {
	if r.Level == nil {
		return ""
	}
	return *r.Level
}

type LeaderboardEntry struct {
	Place int `json:"place"`
	Run   Run `json:"run"`
}

type Leaderboard struct {
	Weblink  string             `json:"weblink"`
	Game     string             `json:"game"`
	Category string             `json:"category"`
	Level    *string            `json:"level"`
	Timing   string             `json:"timing"`
	Values   map[string]string  `json:"values"`
	Runs     []LeaderboardEntry `json:"runs"`
	Players  Players            `json:"players"`
}
