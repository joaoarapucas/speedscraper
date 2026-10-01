# Resumo das respostas JSON — speedrun.com API v1

Resumo de todos os formatos JSON retornados pelos endpoints documentados em [`version1/`](version1/README.md).
Base URL: `https://www.speedrun.com/api/v1`.

* [1. Convenções gerais](#1-convenções-gerais)
* [2. Endpoints e tipo de retorno](#2-endpoints-e-tipo-de-retorno)
* [3. Estruturas dos recursos](#3-estruturas-dos-recursos)

---

## 1. Convenções gerais

### Envelope `data`

Toda resposta de sucesso é envelopada em `data`:

```json
{ "data": <recurso> }              // recurso único
{ "data": [<recurso>, ...] }       // coleção
```

### Paginação

Coleções paginadas incluem também o elemento `pagination` (padrão 20 itens, máximo 200 via `max`;
1000 em modo bulk de games). Ver [pagination.md](version1/pagination.md).

```json
{
  "data": [<user>, <user>, <user>],
  "pagination": {
    "offset": 20,
    "max": 30,
    "size": 30,
    "links": [
      { "rel": "next", "uri": "https://www.speedrun.com/api/v1/users?max=30&offset=50" },
      { "rel": "prev", "uri": "https://www.speedrun.com/api/v1/users?max=30" }
    ]
  }
}
```

| Campo    | Tipo  | Descrição                                                   |
| -------- | ----- | ----------------------------------------------------------- |
| `offset` | int   | Posição inicial da página                                   |
| `max`    | int   | Limite de itens solicitado                                  |
| `size`   | int   | Itens efetivamente retornados (`0..max`); `size < max` indica última página |
| `links`  | array | Links `next` / `prev`, quando existirem                     |

A API não informa o total de elementos.

### Links

Quase todo recurso possui `links`: lista de objetos `{ "rel": string, "uri": string }` apontando
para recursos relacionados. `weblink` é a URL do site para humanos.

### Embeds

O parâmetro `?embed=a,b.c` inclui recursos relacionados dentro do próprio objeto, sempre no formato
`"<nome>": { "data": ... }`. Se o campo já existir (ex.: `regions` como lista de IDs), ele é
substituído pelo objeto embedado. Aninhamento limitado a 2 níveis. Ver [embedding.md](version1/embedding.md).

```json
"categories": { "data": [<category>, ...] }
```

### Erros

```json
{
  "status": 400,
  "message": "The submitted run does not validate against the schema. See the `errors` attached.",
  "errors": [
    "[category] is missing and it is required"
  ],
  "links": [
    { "rel": "support", "uri": "irc://speedrunslive.com#speedrun.com" },
    { "rel": "report-issues", "uri": "https://github.com/speedruncom/api/issues" }
  ]
}
```

### Formatos

* Datas e horários: ISO 8601 (`"2014-06-01"`, `"2015-05-23T16:12:32Z"`).
* Durações: ISO 8601 (`"PT31M22S"`) e, em paralelo, segundos como número (`*_t`).
* JSONP: `?callback=foo` retorna `foo({...})` em vez de JSON puro.

---

## 2. Endpoints e tipo de retorno

Legenda: **Auth** = requer API key; **Pag.** = coleção paginada.

| Método | Endpoint                                         | Retorno (`data`)                          | Auth | Pag. |
| ------ | ------------------------------------------------ | ----------------------------------------- | :--: | :--: |
| GET    | `/categories/{id}`                               | [`<category>`](#category)                 |      |      |
| GET    | `/categories/{id}/variables`                     | [`<variable>[]`](#variable)               |      |      |
| GET    | `/categories/{id}/records`                       | [`<leaderboard>[]`](#leaderboard)         |      |  ✓   |
| GET    | `/developers`                                    | [`<developer>[]`](#recursos-simples)      |      |  ✓   |
| GET    | `/developers/{id}`                               | [`<developer>`](#recursos-simples)        |      |      |
| GET    | `/engines`                                       | [`<engine>[]`](#recursos-simples)         |      |  ✓   |
| GET    | `/engines/{id}`                                  | [`<engine>`](#recursos-simples)           |      |      |
| GET    | `/games`                                         | [`<game>[]`](#game) ou [`<game-bulk>[]`](#game-bulk) |  |  ✓   |
| GET    | `/games/{id}`                                    | [`<game>`](#game)                         |      |      |
| GET    | `/games/{id}/categories`                         | [`<category>[]`](#category)               |      |      |
| GET    | `/games/{id}/levels`                             | [`<level>[]`](#level)                     |      |      |
| GET    | `/games/{id}/variables`                          | [`<variable>[]`](#variable)               |      |      |
| GET    | `/games/{id}/derived-games` (alias `/romhacks`)  | [`<game>[]`](#game)                       |      |  ✓   |
| GET    | `/games/{id}/records`                            | [`<leaderboard>[]`](#leaderboard)         |      |  ✓   |
| GET    | `/gametypes`                                     | [`<gametype>[]`](#recursos-simples)       |      |  ✓   |
| GET    | `/gametypes/{id}`                                | [`<gametype>`](#recursos-simples)         |      |      |
| GET    | `/genres`                                        | [`<genre>[]`](#recursos-simples)          |      |  ✓   |
| GET    | `/genres/{id}`                                   | [`<genre>`](#recursos-simples)            |      |      |
| GET    | `/guests/{name}`                                 | [`<guest>`](#guest)                       |      |      |
| GET    | `/leaderboards/{game}/category/{category}`       | [`<leaderboard>`](#leaderboard)           |      |      |
| GET    | `/leaderboards/{game}/level/{level}/{category}`  | [`<leaderboard>`](#leaderboard)           |      |      |
| GET    | `/levels/{id}`                                   | [`<level>`](#level)                       |      |      |
| GET    | `/levels/{id}/categories`                        | [`<category>[]`](#category)               |      |      |
| GET    | `/levels/{id}/variables`                         | [`<variable>[]`](#variable)               |      |      |
| GET    | `/levels/{id}/records`                           | [`<leaderboard>[]`](#leaderboard)         |      |  ✓   |
| GET    | `/notifications`                                 | [`<notification>[]`](#notification)       |  ✓   |  ✓   |
| GET    | `/platforms`                                     | [`<platform>[]`](#platform)               |      |  ✓   |
| GET    | `/platforms/{id}`                                | [`<platform>`](#platform)                 |      |      |
| GET    | `/profile`                                       | [`<profile>`](#profile)                   |  ✓   |      |
| GET    | `/publishers`                                    | [`<publisher>[]`](#recursos-simples)      |      |  ✓   |
| GET    | `/publishers/{id}`                               | [`<publisher>`](#recursos-simples)        |      |      |
| GET    | `/regions`                                       | [`<region>[]`](#region)                   |      |  ✓   |
| GET    | `/regions/{id}`                                  | [`<region>`](#region)                     |      |      |
| GET    | `/runs`                                          | [`<run>[]`](#run)                         |      |  ✓   |
| GET    | `/runs/{id}`                                     | [`<run>`](#run)                           |      |      |
| POST   | `/runs`                                          | [`<run>`](#run) — HTTP 201 + header `Location`; erro 400 no [formato de erro](#erros) | ✓ | |
| PUT    | `/runs/{id}/status`                              | Não documentado (presumivelmente `<run>`) |  ✓   |      |
| PUT    | `/runs/{id}/players`                             | [`<run>`](#run) atualizado                |  ✓   |      |
| DELETE | `/runs/{id}`                                     | [`<run>`](#run) removido                  |  ✓   |      |
| GET    | `/series`                                        | [`<series>[]`](#series)                   |      |  ✓   |
| GET    | `/series/{id}`                                   | [`<series>`](#series)                     |      |      |
| GET    | `/series/{id}/games`                             | [`<game>[]`](#game) ou [`<game-bulk>[]`](#game-bulk) |  |  ✓   |
| GET    | `/users`                                         | [`<user>[]`](#user)                       |      |  ✓   |
| GET    | `/users/{id}`                                    | [`<user>`](#user)                         |      |      |
| GET    | `/users/{id}/personal-bests`                     | [`<ranked-run>[]`](#ranked-run-personal-bests) |  |      |
| GET    | `/variables/{id}`                                | [`<variable>`](#variable)                 |      |      |

> A coluna **Pag.** marca os endpoints que a documentação declara explicitamente como paginados
> (`*/records`) e as coleções gerais (listas globais), que seguem a regra geral de paginação do
> [README da v1](version1/README.md). Em caso de dúvida, verifique a presença de `pagination` na resposta.

---

## 3. Estruturas dos recursos

### Recursos simples

`developer`, `engine`, `gametype`, `genre` e `publisher` compartilham a mesma estrutura:

```json
{
  "id": "l4eprzro",
  "name": "Arkane Studios",
  "links": [
    { "rel": "self",  "uri": ".../developers/l4eprzro" },
    { "rel": "games", "uri": ".../games?developer=l4eprzro" }
  ]
}
```

| Campo   | Tipo   | Descrição         |
| ------- | ------ | ----------------- |
| `id`    | string | ID do recurso     |
| `name`  | string | Nome              |
| `links` | array  | `self`, `games`   |

### Region

```json
{
  "id": "pr184lqn",
  "name": "USA / NTSC",
  "links": [ "self", "games", "runs" ]
}
```

### Platform

```json
{
  "id": "rdjq4vwe",
  "name": "Nintendo Entertainment System",
  "released": 2002,
  "links": [ "self", "games", "runs" ]
}
```

| Campo      | Tipo | Descrição                     |
| ---------- | ---- | ----------------------------- |
| `released` | int  | Ano de lançamento da plataforma |

### Guest

Jogador sem conta no site.

```json
{
  "name": "Alex",
  "links": [ "self", "runs" ]
}
```

### Game

```json
{
  "id": "1kgr75w4",
  "names": { "international": "Super Mario Sunshine", "japanese": "...", "twitch": "Super Mario Sunshine" },
  "abbreviation": "sms",
  "weblink": "https://www.speedrun.com/sms",
  "released": 2002,
  "release-date": "2002-08-26",
  "ruleset": {
    "show-milliseconds": false,
    "require-verification": true,
    "require-video": false,
    "run-times": ["realtime", "realtime_noloads"],
    "default-time": "realtime",
    "emulators-allowed": false
  },
  "romhack": false,
  "gametypes": [],
  "platforms": ["039or978", "hdzate15"],
  "regions": ["hdz48alk", "hdz1hdwo"],
  "genres": [],
  "engines": [],
  "developers": [],
  "publishers": [],
  "moderators": { "wzx7q875": "moderator", "zzb12med": "super-moderator" },
  "created": "2014-12-07T12:50:20Z",
  "assets": { "<nome>": { "uri": "...", "width": 180, "height": 34 } },
  "links": [ "self", "runs", "levels", "categories", "variables", "records",
             "series", "base-game", "derived-games", "romhacks", "leaderboard" ]
}
```

| Campo                     | Tipo            | Observações |
| ------------------------- | --------------- | ----------- |
| `names.international`     | string          | |
| `names.japanese`          | string \| null  | |
| `names.twitch`            | string          | |
| `abbreviation`            | string          | Não usar para montar URLs; use `weblink` |
| `released`                | int             | Legado; substituído por `release-date` |
| `release-date`            | string (data)   | |
| `ruleset.run-times`       | string[]        | Combinação de `realtime`, `realtime_noloads`, `ingame` |
| `ruleset.default-time`    | string          | Tempo primário |
| `romhack`                 | bool            | Legado; substituído por `gametypes` |
| `gametypes`, `platforms`, `regions`, `genres`, `engines`, `developers`, `publishers` | string[] | Listas de IDs (podem ser vazias); substituídas por `{ "data": [...] }` com embed |
| `moderators`              | object          | `userID → "moderator" \| "super-moderator"` |
| `created`                 | string \| null  | `null` para jogos antigos |
| `assets`                  | object          | Chaves: `logo`, `cover-tiny`, `cover-small`, `cover-medium`, `cover-large`, `icon`, `trophy-1st`, `trophy-2nd`, `trophy-3rd`, `trophy-4th`, `background`, `foreground`. Apenas `trophy-4th`, `background` e `foreground` podem ser `null` |
| `links`                   | array           | `base-game` só existe se houver jogo base; `series` pode aparecer várias vezes; `romhacks` é legado |

**Embeds:** `levels`, `categories`, `moderators`, `gametypes`, `platforms`, `regions`, `genres`,
`engines`, `developers`, `publishers`, `variables`.

#### Game (bulk)

Retornado por `GET /games?_bulk=yes` e `GET /series/{id}/games?_bulk=yes`. Sem embeds.

```json
{
  "id": "1kgr75w4",
  "names": { "international": "Super Mario Sunshine", "japanese": "..." },
  "abbreviation": "sms",
  "weblink": "https://www.speedrun.com/sms"
}
```

### Series

```json
{
  "id": "1kgr75w4",
  "names": { "international": "Grand Theft Auto", "japanese": "" },
  "abbreviation": "gta",
  "weblink": "https://www.speedrun.com/gta",
  "moderators": { "wzx7q875": "moderator" },
  "created": "2014-12-07T12:50:20Z",
  "assets": { "...": "mesmo formato de game.assets" },
  "links": [ "self", "games" ]
}
```

`names.japanese` e `created` podem ser `null`. **Embeds:** `moderators`.

### Category

```json
{
  "id": "jkh473tf",
  "name": "Any%",
  "weblink": "https://www.speedrun.com/tww#Anypc",
  "type": "per-game",
  "rules": "Yada Yada Yada",
  "players": { "type": "exactly", "value": 2 },
  "miscellaneous": false,
  "links": [ "self", "game", "variables", "runs" ]
}
```

| Campo           | Tipo   | Valores / observações |
| --------------- | ------ | --------------------- |
| `type`          | string | `per-game` \| `per-level` |
| `rules`         | string | Texto livre com markup próprio do site |
| `players.type`  | string | `exactly` \| `up-to` |
| `players.value` | int    | Número de participantes |
| `miscellaneous` | bool   | |
| `weblink`       | string | Para `per-level`, aponta para o jogo (exceto quando obtido via `/levels/{id}/categories`) |

**Embeds:** `game`, `variables`.

### Level

```json
{
  "id": "ha71kldd",
  "name": "Slip Slide Icecapades",
  "weblink": "https://www.speedrun.com/twinsanity/Slip_Slide_Icecapades",
  "rules": "Yada Yada Yada",
  "links": [ "self", "game", "categories", "variables", "runs" ]
}
```

**Embeds:** `categories` (somente `per-level`), `variables`.

### Variable

```json
{
  "id": "wzx7q875",
  "name": "cc",
  "category": null,
  "scope": { "type": "full-game" },
  "mandatory": true,
  "user-defined": false,
  "obsoletes": false,
  "values": {
    "values": {
      "zdbx1h88": { "label": "150cc", "rules": "do not cheat", "flags": { "miscellaneous": false } }
    },
    "default": "zdbx1h88"
  },
  "is-subcategory": true,
  "links": [ "self", "game" ]
}
```

| Campo            | Tipo           | Valores / observações |
| ---------------- | -------------- | --------------------- |
| `category`       | string \| null | `null` = aplica-se a todas as categorias |
| `scope.type`     | string         | `global` \| `full-game` \| `all-levels` \| `single-level` |
| `scope.level`    | string         | Presente apenas quando `scope.type = single-level` |
| `mandatory`      | bool           | Runs novas precisam informar valor |
| `user-defined`   | bool           | Permite valor customizado |
| `obsoletes`      | bool           | Considerada ao montar leaderboards |
| `values.values`  | object         | `valueID → { label, rules?, flags? }`; `rules` e `flags` só existem em subcategorias |
| `values.default` | string \| null | ID do valor padrão |
| `is-subcategory` | bool           | Dica de interface |

### Run

```json
{
  "id": "90y6pm7e",
  "weblink": "https://www.speedrun.com/run/90y6pm7e",
  "game": "29d30dlp",
  "level": null,
  "category": "nxd1rk8q",
  "videos": {
    "text": "Part 1: http://youtube.com/videoidhere -- HYPE!!",
    "links": [ { "uri": "https://youtube.com/videoidhere" } ]
  },
  "comment": "...",
  "status": { "status": "verified", "examiner": "61xymxr9", "verify-date": "2015-05-23T16:12:32Z" },
  "players": [
    { "rel": "user",  "id": "61xymxr9",    "uri": ".../users/61xymxr9" },
    { "rel": "guest", "name": "Betsruner", "uri": ".../guests/Betsruner" }
  ],
  "date": "2014-06-01",
  "submitted": null,
  "times": {
    "primary": "PT31M22S",          "primary_t": 1882,
    "realtime": "PT31M22S",         "realtime_t": 1882,
    "realtime_noloads": "PT41M26S", "realtime_noloads_t": 2486,
    "ingame": null,                 "ingame_t": 0
  },
  "system": { "platform": "rdjq4vwe", "emulated": false, "region": null },
  "splits": { "rel": "splits.io", "uri": "https://splits.io/api/v3/runs/55b" },
  "values": { "wvn9wp51": "ezj43ozx" },
  "links": [ "self", "game", "category", "level", "platform", "examiner" ]
}
```

| Campo                | Tipo                    | Valores / observações |
| -------------------- | ----------------------- | --------------------- |
| `level`              | string \| null          | `null` para runs de jogo completo |
| `videos`             | object \| null          | `null` se não houver vídeo nem texto; `videos.text` só existe se o link primário não for apenas uma URL |
| `status.status`      | string                  | `new` \| `verified` \| `rejected` |
| `status.examiner`    | string                  | Ausente quando `new` |
| `status.verify-date` | string \| null          | Apenas em `verified`; pode ser `null` em runs antigas |
| `status.reason`      | string                  | Apenas em `rejected` |
| `players[]`          | object                  | `rel: "user"` → `id`; `rel: "guest"` → `name`; ambos têm `uri` |
| `date`               | string \| null          | Data em que a run ocorreu |
| `submitted`          | string \| null          | Data/hora de submissão |
| `times.*`            | string (ISO) / number   | `primary` sempre preenchido; os demais dependem do jogo. Sufixo `_t` = segundos (float) |
| `system.region`      | string \| null          | Comum ser `null` em jogos de PC |
| `splits`             | object \| null          | Link para os splits |
| `values`             | object                  | `variableID → valueID` |

**Embeds:** `game`, `category`, `level`, `players` (substitui `players` por usuários/guests completos,
mantendo `rel`), `region`, `platform`.

### Ranked run (personal bests)

Retornado por `GET /users/{id}/personal-bests`. Embeds de run ficam **ao lado** de `run`, não dentro:

```json
{
  "data": [
    { "place": 7, "run": <run>, "game": <game>, "category": <category> },
    { "place": 2, "run": <run> }
  ]
}
```

### Leaderboard

```json
{
  "weblink": "https://www.speedrun.com/.hackInfection#Any",
  "game": "k6qrzm1g",
  "category": "jdr3y0d6",
  "level": null,
  "platform": null,
  "region": null,
  "emulators": null,
  "video-only": false,
  "timing": "ingame",
  "values": {},
  "runs": [
    { "place": 1, "run": <run> },
    { "place": 2, "run": <run> },
    { "place": 2, "run": <run> }
  ],
  "links": [ "game", "category" ]
}
```

| Campo                    | Tipo            | Observações |
| ------------------------ | --------------- | ----------- |
| `game`, `category`       | string          | Sempre presentes |
| `level`                  | string \| null  | Preenchido em leaderboards de nível individual |
| `platform`, `region`     | string \| null  | Filtros aplicados (IDs) |
| `emulators`              | bool \| null    | Filtro aplicado |
| `video-only`             | bool            | Nunca `null` |
| `timing`                 | string          | `realtime` \| `realtime_noloads` \| `ingame` |
| `values`                 | object          | Filtros de variáveis: `variableID → valueID` |
| `runs[]`                 | object          | `{ place, run }`; várias runs podem ter o mesmo `place` |

**Embeds:** `game`, `category`, `level`, `players` (lista plana no nível do leaderboard), `regions`,
`platforms`, `variables`. Não é possível embedar dentro das runs do leaderboard.

### User

```json
{
  "id": "kjpdr4jq",
  "names": { "international": "chewdiggy", "japanese": null },
  "weblink": "https://www.speedrun.com/user/chewdiggy",
  "name-style": {
    "style": "solid",
    "color": { "light": "#249BCE", "dark": "#44BBEE" }
  },
  "role": "user",
  "signup": "2014-10-02T12:34:23Z",
  "location": {
    "country": { "code": "GB", "names": { "international": "United Kingdom", "japanese": "..." } },
    "region":  { "code": "ENGLAND", "names": { "international": "England", "japanese": "..." } }
  },
  "twitch":        { "uri": "https://www.twitch.tv/username" },
  "hitbox":        { "uri": "https://www.hitbox.tv/username" },
  "youtube":       { "uri": "https://www.youtube.com/..." },
  "twitter":       { "uri": "https://www.twitter.com/username" },
  "speedrunslive": { "uri": "http://www.speedrunslive.com/profiles/#!/username" },
  "links": [ "self", "runs", "games", "personal-bests" ]
}
```

| Campo              | Tipo            | Valores / observações |
| ------------------ | --------------- | --------------------- |
| `name-style.style` | string          | `solid` (campo `color`) \| `gradient` (campos `color-from` e `color-to`); cada cor tem `light` e `dark` |
| `role`             | string          | `banned` \| `user` \| `trusted` \| `moderator` \| `admin` \| `programmer` |
| `signup`           | string \| null  | `null` para contas antigas |
| `location`         | object \| null  | `country` sempre presente quando há localização; `region` é opcional |
| `twitch`, `hitbox`, `youtube`, `twitter`, `speedrunslive` | object \| null | `{ uri }` |

### Profile

Idêntico a [`<user>`](#user), com duas diferenças em `links`: `self` aponta para `/profile` e há um
link adicional `user` apontando para o recurso do usuário.

### Notification

```json
{
  "id": "p2p5rrle",
  "created": "2015-01-25T11:55:15Z",
  "status": "read",
  "text": "Lighnat0r likes your post.",
  "item": { "rel": "post", "uri": "https://www.speedrun.com/post/ptv8h" },
  "links": [
    { "rel": "run",  "uri": ".../runs/1wzpqgyq" },
    { "rel": "game", "uri": ".../games/nj1ne1p4" }
  ]
}
```

| Campo      | Tipo   | Valores / observações |
| ---------- | ------ | --------------------- |
| `status`   | string | `read` \| `unread` |
| `item.rel` | string | `post` \| `run` \| `game` \| `guide`; se o item não existir mais, `uri` aponta para a home |
| `links`    | array  | No máximo um link (`run` ou `game`), podendo não existir |
