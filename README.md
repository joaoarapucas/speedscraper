# speedscraper

Scraper do [speedrun.com](https://www.speedrun.com) escrito em Go que coleta jogos, metadados,
usuários, runs (verificadas, pendentes e rejeitadas) e leaderboards para um banco SQLite, e depois
continua acompanhando o site, buscando apenas o que mudou.

O formato das respostas da API está resumido em [RESUMO-RESPOSTAS-JSON.md](RESUMO-RESPOSTAS-JSON.md).

## Como funciona

### Fases

O daemon é uma fila de jobs persistida no próprio SQLite. A ordem de execução vem de faixas de
prioridade, e dentro de cada faixa os jogos maiores vêm primeiro:

| Fase | O que faz | Custo aproximado |
|---|---|---|
| 0. Descoberta | lista todos os jogos (`/games?_bulk=yes`, 1000 por página), séries e lê as ~10 mil runs mais recentes para estimar a atividade de cada jogo | ~200 requisições |
| 1. Metadados | 1 requisição por jogo com categorias, níveis, variáveis, plataformas, gêneros etc. embutidos | ~52 mil requisições (~10 h) |
| 2. Runs | todas as runs de cada jogo, com os jogadores embutidos (usuários vêm de graça) | ~1 requisição por jogo + 1 por 200 runs |
| 3. Leaderboards | 1 requisição por categoria (ou categoria × nível) com pelo menos uma run verificada | gargalo da carga inicial |
| Regime permanente | feed de runs verificadas a cada 5 min, boards alteradas, jogos novos e reconciliação periódica | poucos milhares por dia |

Cada troca de fase gera um e-mail com o relatório de progresso.

### Leaderboards sem desperdício

* **Uma requisição por board, não por subcategoria.** O endpoint `/leaderboards` sem filtros `var-`
  devolve a união de todas as sub-boards de subcategoria. O scraper busca essa união e deriva
  cada sub-board localmente a partir dos valores das variáveis de subcategoria de cada run
  (validado contra as boards filtradas oficiais).
* **Só busca o que mudou.** O feed `/runs?status=verified&orderby=verify-date` é lido a cada
  5 minutos; cada run que passou a ser verificada (ou mudou de tempo) marca sua board como suja,
  e a board é rebuscada 10 minutos depois (agrupando várias verificações seguidas). Boards
  sem novidades não são consultadas.
* **Só grava o que mudou.** A tabela `board_runs` guarda o *intervalo* em que cada run esteve na
  board (`valid_from`/`valid_to`). Um novo recorde gera uma linha nova e fecha a linha do PB
  antigo; o resto da board não é tocado. As posições são derivadas por `RANK()` sobre o tempo, então
  uma run nova no 30º lugar não reescreve do 31º em diante. Se o hash da board não mudou, nada é
  escrito além de `last_fetched_at`.
* **Boards grandes demais para a API.** Em boards enormes (por exemplo Subway Surfers) a API estoura
  o próprio timeout e devolve HTTP 500, mesmo com `top=`. O scraper então busca o board em fatias:
  cada subcategoria (`var-`); a fatia que ainda falhar é dividida por plataforma (`platform=`), e
  depois por emulador (`emulators=`). Em cada subcategoria fica a melhor run de cada jogador, e o
  empate de tempo vai para a run mais antiga, como no site. Uma fatia que falha mesmo no menor
  recorte é ranqueada a partir das runs do banco. Ela fica exata se o crawl de runs do jogo não
  foi truncado. Esses boards ficam com `boards.split = 1`, e `boards.computed_slices` conta as
  fatias calculadas localmente. Daí em diante são sempre buscados em fatias, e chega um e-mail
  na primeira vez.
* **Reconciliação.** O feed não enxerga runs removidas ou rejeitadas depois de verificadas. Por isso
  boards e runs são rebuscadas periodicamente usando a cota ociosa: a cada 7 dias para jogos com
  mais de 1000 runs, a cada 30 dias para os demais. Runs que somem num recrawl completo recebem
  `deleted_at`.

### Limites da API

* 100 requisições/minuto por IP. O scraper usa 90 por padrão (`--rpm`), com um token bucket global
  compartilhado pelos workers. HTTP 420 pausa todas as requisições por 60 s.
* `/runs` não pagina além de 10.000 itens. Partições maiores são divididas por status, categoria e
  nível; se ainda assim passarem do limite, são lidas pelas duas pontas (até 20 mil) e o jogo fica
  marcado com `runs_truncated = 1`.

## Compilação

Requer Go 1.26. O driver SQLite é Go puro, então não há CGO e o binário é estático.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/speedscraper ./cmd/speedscraper
```

Para Raspberry Pi e similares use `GOARCH=arm64`. O mesmo código compila para Windows (`GOOS=windows`).

Testes:

```bash
go test ./...
```

## Instalação no Debian (systemd)

Primeiro compile o binário na raiz do repositório (gera `dist/speedscraper`). É preciso Go 1.26:
o pacote `golang-go` do Debian costuma ser mais antigo, então instale pelo tarball oficial de
<https://go.dev/dl/> se `go version` mostrar uma versão menor.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/speedscraper ./cmd/speedscraper
```

Depois instale:

```bash
sudo useradd --system --home /var/lib/speedscraper --shell /usr/sbin/nologin speedscraper
sudo install -d -o speedscraper -g speedscraper /var/lib/speedscraper
sudo install -m 755 dist/speedscraper /usr/local/bin/speedscraper
sudo install -d -m 755 /etc/speedscraper
sudo install -m 600 deploy/env.example /etc/speedscraper/env   # depois edite com seus dados SMTP
sudo install -m 644 deploy/speedscraper.service deploy/speedscraper-alert@.service /etc/systemd/system/
sudo systemctl daemon-reload
```

Teste o e-mail antes de ligar o daemon:

```bash
sudo sh -c 'set -a; . /etc/speedscraper/env; /usr/local/bin/speedscraper notify-test'
```

Ligue e acompanhe:

```bash
sudo systemctl enable --now speedscraper
journalctl -u speedscraper -f
sudo -u speedscraper speedscraper status --db /var/lib/speedscraper/speedscraper.db
```

O processo pode ser parado e reiniciado a qualquer momento: o estado está todo no banco, jobs
interrompidos voltam para a fila e o trabalho continua de onde parou.

## Alertas por e-mail

Configurados pelas variáveis `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM` e
`ALERT_TO` (veja [deploy/env.example](deploy/env.example)). Sem elas os alertas vão apenas para o log.

| Evento | Severidade |
|---|---|
| Fase concluída e início da próxima; carga inicial completa | INFO |
| Daemon iniciado | INFO |
| Resumo diário às 8h (`--digest-hour`) | INFO |
| Reinício após encerramento inesperado (queda de energia, `kill -9`) | AVISO |
| Mais de 20 respostas 420 em 10 min, ou mais de 10% de erros em 15 min | AVISO |
| Job que falhou definitivamente (após 8 tentativas); `requeue-failed` o devolve à fila | AVISO |
| Leaderboard grande demais para a API, passando a ser buscado em fatias | AVISO |
| Feed atrasado além do limite de paginação | AVISO |
| Disco com menos de 5 GiB livres (`--min-free-gb`) | AVISO |
| Nenhum job concluído em 30 min | CRÍTICO |
| Feed de runs verificadas parado há mais de 1 hora | CRÍTICO |
| Erro grave do SQLite (disco cheio, corrupção, I/O) | CRÍTICO |
| systemd desistiu de reiniciar o serviço (via `speedscraper-alert@.service`) | CRÍTICO |

Cada tipo de alerta tem um intervalo mínimo de 1 hora entre envios.

## Uso

```
speedscraper run         [flags]   daemon (carga inicial + acompanhamento)
speedscraper status      [flags]   progresso, contagens e fila de jobs
speedscraper once KIND [KEY]       executa um único job (depuração)
speedscraper migrate     [flags]   cria/atualiza o esquema
speedscraper requeue-failed [KIND] devolve à fila os jobs que falharam definitivamente
speedscraper notify-test           envia um e-mail de teste
speedscraper alert -subject S      envia um e-mail com o corpo lido da entrada padrão
```

Flags principais de `run`: `--db`, `--rpm`, `--workers`, `--feed-interval`, `--dirty-delay`,
`--hot-runs`, `--hot-refresh`, `--cold-refresh`, `--digest-hour`, `--min-free-gb`,
`--log-format json`. Para testar com poucos jogos: `--only-games sms,thps1` (IDs ou abreviações;
desliga a descoberta e os feeds).

## Consultas de exemplo

Leaderboard atual de uma subcategoria:

```sql
SELECT place, u.name, r.primary_t, r.date
FROM board_ranking_current br
JOIN run_players rp ON rp.run_id = br.run_id AND rp.ord = 0
LEFT JOIN users u ON u.id = rp.user_id
JOIN runs r ON r.id = br.run_id
WHERE br.game_id = 'jy65g3de' AND br.category_id = 'xk901gk0' AND br.subcat_key = 'dloxmjjl=q8kj0wgq'
ORDER BY place;
```

Leaderboard como estava numa data (dentro do período coletado):

```sql
SELECT br.board_id, br.subcat_key, br.run_id,
       RANK() OVER (PARTITION BY br.board_id, br.subcat_key ORDER BY r.primary_t) AS place
FROM board_runs br JOIN runs r ON r.id = br.run_id
WHERE br.valid_from <= '2026-11-15T00:00:00Z'
  AND (br.valid_to IS NULL OR br.valid_to > '2026-11-15T00:00:00Z');
```

`valid_from` marca quando o scraper viu a run na board pela primeira vez. Para a história anterior
ao início da coleta, use `runs.verify_date` e `runs.date`.

Tempo entre submissão e verificação por jogo:

```sql
SELECT game_id, COUNT(*) AS runs,
       AVG(julianday(verify_date) - julianday(submitted)) AS dias_medios
FROM runs
WHERE status = 'verified' AND verify_date IS NOT NULL AND submitted IS NOT NULL
GROUP BY game_id ORDER BY runs DESC LIMIT 20;
```

## Estrutura

```
cmd/speedscraper/   CLI
internal/api/       cliente HTTP: rate limit global, retries, paginação, tipos
internal/store/     SQLite: migrações, upserts, armazenamento das boards por diff
internal/board/     chave de board, resolução de subcategorias, diff (funções puras)
internal/scheduler/ fila de jobs persistente e pool de workers
internal/jobs/      descoberta, metadados, crawl de runs, boards e feeds
internal/monitor/   fases, watchdog, relatório de status
internal/notify/    e-mail (SMTP) com deduplicação
deploy/             units do systemd e exemplo de configuração
```

## Licença dos dados

O conteúdo do speedrun.com é licenciado sob [CC-BY-NC 4.0](https://creativecommons.org/licenses/by-nc/4.0/).
