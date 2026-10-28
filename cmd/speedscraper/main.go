// Command speedscraper scrapes speedrun.com into a SQLite database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/jobs"
	"github.com/joaoarapucas/speedscraper/internal/monitor"
	"github.com/joaoarapucas/speedscraper/internal/notify"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

var version = "0.2.0"

const usage = `speedscraper - scraper do speedrun.com para SQLite

Uso:
  speedscraper run         [flags]   executa o daemon (carga inicial + acompanhamento)
  speedscraper status      [flags]   mostra o progresso e a fila de jobs
  speedscraper once KIND [KEY]       executa um unico job (depuracao)
  speedscraper migrate     [flags]   cria/atualiza o esquema do banco
  speedscraper requeue-failed [KIND] devolve a fila os jobs que falharam definitivamente
  speedscraper notify-test           envia um e-mail de teste
  speedscraper alert -subject S      envia um e-mail com o corpo lido da entrada padrao
  speedscraper version

Variaveis de ambiente: SPEEDSCRAPER_DB, SMTP_HOST, SMTP_PORT, SMTP_USER, SMTP_PASS,
SMTP_FROM, ALERT_TO. Use "speedscraper <comando> -h" para ver as flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "once":
		err = cmdOnce(args)
	case "migrate":
		err = cmdMigrate(args)
	case "requeue-failed":
		err = cmdRequeueFailed(args)
	case "notify-test":
		err = cmdNotifyTest(args)
	case "alert":
		err = cmdAlert(args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "comando desconhecido %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

type common struct {
	db        string
	logLevel  string
	logFormat string
	baseURL   string
	rpm       int
}

func (c *common) register(fs *flag.FlagSet) {
	def := os.Getenv("SPEEDSCRAPER_DB")
	if def == "" {
		def = "speedscraper.db"
	}
	fs.StringVar(&c.db, "db", def, "caminho do banco SQLite")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn ou error")
	fs.StringVar(&c.logFormat, "log-format", "text", "text ou json")
	fs.StringVar(&c.baseURL, "base-url", api.DefaultBaseURL, "URL base da API")
	fs.IntVar(&c.rpm, "rpm", 90, "requisicoes por minuto (limite da API: 100)")
}

func (c *common) logger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if c.logFormat == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func (c *common) client(log *slog.Logger) *api.Client {
	return api.New(api.Options{
		BaseURL:   c.baseURL,
		UserAgent: "speedscraper/" + version,
		Limiter:   api.NewLimiter(c.rpm),
		Logger:    log,
	})
}

func newAlerter(db *store.DB, log *slog.Logger) (*notify.Alerter, bool, error) {
	cfg, ok, err := notify.FromEnv()
	if err != nil {
		return nil, false, err
	}
	var n notify.Notifier = notify.LogNotifier{Log: log}
	if ok {
		n = notify.SMTP{Cfg: cfg}
	}
	host, _ := os.Hostname()
	return &notify.Alerter{N: n, Store: db, Log: log, Cooldown: time.Hour, Prefix: "[speedscraper@" + host + "]",
		Retries: 3, RetryGap: 30 * time.Second}, ok, nil
}

const checkpointRunning = "daemon_running"

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	cfg := jobs.DefaultConfig()
	mcfg := monitor.DefaultConfig()
	workers := fs.Int("workers", 6, "workers concorrentes (escondem latencia; a cota e global)")
	grace := fs.Duration("grace", 30*time.Second, "tempo para jobs em andamento terminarem no desligamento")
	onlyGames := fs.String("only-games", "", "restringe a estes jogos (IDs ou abreviacoes, separados por virgula); desliga feeds e descoberta")
	minFreeGB := fs.Float64("min-free-gb", 5, "alerta quando o disco do banco tiver menos que isso livre (0 desliga)")
	fs.DurationVar(&cfg.FeedInterval, "feed-interval", cfg.FeedInterval, "intervalo de leitura dos feeds de runs")
	fs.DurationVar(&cfg.DirtyDelay, "dirty-delay", cfg.DirtyDelay, "espera antes de rebuscar uma board alterada (agrupa mudancas)")
	fs.IntVar(&cfg.HotRunCount, "hot-runs", cfg.HotRunCount, "jogos com pelo menos estas runs sao reconciliados com mais frequencia")
	fs.DurationVar(&cfg.HotRefresh, "hot-refresh", cfg.HotRefresh, "periodo de reconciliacao dos jogos grandes")
	fs.DurationVar(&cfg.ColdRefresh, "cold-refresh", cfg.ColdRefresh, "periodo de reconciliacao dos demais jogos")
	fs.IntVar(&mcfg.DigestHour, "digest-hour", mcfg.DigestHour, "hora local do resumo diario por e-mail (-1 desliga)")
	_ = fs.Parse(args)
	if *onlyGames != "" {
		for _, g := range strings.Split(*onlyGames, ",") {
			if g = strings.TrimSpace(g); g != "" {
				cfg.OnlyGames = append(cfg.OnlyGames, g)
			}
		}
	}
	mcfg.FeedsEnabled = len(cfg.OnlyGames) == 0
	mcfg.MinFreeBytes = uint64(*minFreeGB * (1 << 30))

	log := c.logger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, c.db)
	if err != nil {
		return err
	}
	defer db.Close()
	alerter, smtpOK, err := newAlerter(db, log)
	if err != nil {
		return err
	}
	defer alerter.Wait()
	if !smtpOK {
		log.Warn("SMTP not configured (SMTP_HOST/ALERT_TO); alerts will only be logged")
	}

	q := scheduler.NewQueue(db)
	if n, err := q.ResetRunning(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Info("recovered jobs left running", "jobs", n)
	}
	if prev, _, err := db.Checkpoint(ctx, checkpointRunning); err != nil {
		return err
	} else if prev != "" {
		alerter.Alert(ctx, "unclean_restart", notify.Warning, "Reinicio apos encerramento inesperado",
			"O processo anterior ("+prev+") nao terminou de forma limpa (queda de energia, kill -9 ou crash). "+
				"Os jobs interrompidos voltaram para a fila e o trabalho continua de onde parou.")
	}
	host, _ := os.Hostname()
	if err := db.SetCheckpoint(ctx, checkpointRunning, fmt.Sprintf("pid %d em %s desde %s", os.Getpid(), host, db.NowString())); err != nil {
		return err
	}

	client := c.client(log)
	runner := &jobs.Runner{API: client, DB: db, Q: q, Cfg: cfg, Log: log,
		Warn: func(ctx context.Context, kind, subject, body string) {
			alerter.Alert(ctx, kind, notify.Warning, subject, body)
		}}
	if err := runner.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	pool := &scheduler.Pool{Queue: q, Handlers: runner.Handlers(), Workers: *workers, Grace: *grace, Logger: log}
	mon := &monitor.Monitor{DB: db, Q: q, API: client, Pool: pool, Alerter: alerter, Log: log, Cfg: mcfg}
	pool.OnError = mon.OnJobError

	alerter.Alert(ctx, "started", notify.Info, "Scraper iniciado",
		fmt.Sprintf("speedscraper %s iniciado em %s com %d workers e %d req/min.\nBanco: %s", version, host, *workers, c.rpm, db.Path))
	log.Info("speedscraper started", "version", version, "db", db.Path, "workers", *workers, "rpm", c.rpm, "only_games", cfg.OnlyGames)

	monDone := make(chan struct{})
	go func() { mon.Run(ctx); close(monDone) }()
	err = pool.Run(ctx)
	<-monDone
	log.Info("shutting down")
	if derr := db.DeleteCheckpoint(context.WithoutCancel(ctx), checkpointRunning); derr != nil {
		log.Error("clearing running marker failed", "err", derr)
	}
	return err
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	ctx := context.Background()
	db, err := openExisting(ctx, c.db)
	if err != nil {
		return err
	}
	defer db.Close()
	r, err := monitor.Report(ctx, db, scheduler.NewQueue(db))
	if err != nil {
		return err
	}
	fmt.Print(r)
	return nil
}

func cmdOnce(args []string) error {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	var c common
	c.register(fs)
	pos := parseArgs(fs, args)
	if len(pos) < 1 {
		return errors.New("uso: speedscraper once KIND [KEY]")
	}
	kind, key := pos[0], ""
	if len(pos) > 1 {
		key = pos[1]
	}
	log := c.logger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, c.db)
	if err != nil {
		return err
	}
	defer db.Close()
	runner := &jobs.Runner{API: c.client(log), DB: db, Q: scheduler.NewQueue(db), Cfg: jobs.DefaultConfig(), Log: log}
	h, ok := runner.Handlers()[kind]
	if !ok {
		return fmt.Errorf("tipo de job desconhecido %q", kind)
	}
	start := time.Now()
	out, err := h(ctx, &scheduler.Job{Kind: kind, Key: key, Attempts: 1})
	if err != nil {
		return err
	}
	fmt.Printf("ok em %s", time.Since(start).Round(time.Millisecond))
	if !out.Next.IsZero() {
		fmt.Printf(" (proxima execucao seria %s)", out.Next.Format(time.RFC3339))
	}
	fmt.Println()
	return nil
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	db, err := store.Open(context.Background(), c.db)
	if err != nil {
		return err
	}
	fmt.Println("esquema atualizado:", db.Path)
	return db.Close()
}

func cmdRequeueFailed(args []string) error {
	fs := flag.NewFlagSet("requeue-failed", flag.ExitOnError)
	var c common
	c.register(fs)
	pos := parseArgs(fs, args)
	kind := ""
	if len(pos) > 0 {
		kind = pos[0]
	}
	ctx := context.Background()
	db, err := openExisting(ctx, c.db)
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := scheduler.NewQueue(db).RequeueFailed(ctx, kind)
	if err != nil {
		return err
	}
	fmt.Printf("%d job(s) devolvidos a fila\n", n)
	return nil
}

func cmdNotifyTest(args []string) error {
	cfg, ok, err := notify.FromEnv()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("SMTP_HOST e ALERT_TO precisam estar definidos")
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err = notify.SMTP{Cfg: cfg}.Send(ctx, "[speedscraper@"+host+"] e-mail de teste",
		"Se voce recebeu esta mensagem, os alertas do speedscraper estao configurados corretamente.")
	if err != nil {
		return err
	}
	fmt.Println("e-mail enviado para", strings.Join(cfg.To, ", "))
	return nil
}

func cmdAlert(args []string) error {
	fs := flag.NewFlagSet("alert", flag.ExitOnError)
	subject := fs.String("subject", "alerta", "assunto do e-mail")
	_ = fs.Parse(args)
	cfg, ok, err := notify.FromEnv()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("SMTP_HOST e ALERT_TO precisam estar definidos")
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return notify.SMTP{Cfg: cfg}.Send(ctx, "[speedscraper@"+host+"] [CRITICO] "+*subject, string(body))
}

// openExisting opens the database without creating it.
// parseArgs parses flags placed before, between or after positional
// arguments, which the flag package alone stops reading at the first one.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func openExisting(ctx context.Context, path string) (*store.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("banco %s nao encontrado: %w", path, err)
	}
	return store.Open(ctx, path)
}
