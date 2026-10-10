// Package app is the entry point of a C4Stack application. Main runs the app's
// commands: serve, migrate and every operator command the app or its modules register.
//
// The migrate command needs only Config.DatabaseURL and Config.Migrations. It never runs
// Setup, so a migration job needs nothing but database credentials. Every other command
// runs Setup first.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/coolapso/c4stack/internal/postgres"
	"github.com/coolapso/c4stack/web"
)

// Config describes an application to Main.
type Config struct {
	// Name is the app's name, used in help output and logs.
	Name string
	// DatabaseURL is the PostgreSQL connection URL. It is the only setting the migrate
	// command reads.
	DatabaseURL string
	// Migrations are applied in this order by migrate, and checked by readiness. List
	// module migrations first, then the app's own.
	Migrations []Migrations
	// Setup wires the app for every command except migrate: it loads the rest of the
	// configuration, constructs providers and modules, and calls Serve.
	Setup func(ctx context.Context, a *App) error
}

// Migrations are one namespace's migration files: NNN_name.sql at the root of Files.
// Published files must never change; add a new file instead.
type Migrations struct {
	Namespace string
	Files     fs.FS
}

// Command is an operator command run as `<app> <name> [args]`.
type Command struct {
	// Name is one or more words, such as "credits adjust".
	Name  string
	Usage string // arguments, such as "<account-id> <cents>"
	Help  string
	Run   func(ctx context.Context, args []string) error
}

// App is what Setup wires together: the database, the logger, commands, background
// jobs and the server.
type App struct {
	cfg      Config
	db       *sql.DB
	log      *slog.Logger
	commands []Command
	jobs     []job
	server   *web.Server
}

type job struct {
	name     string
	interval time.Duration
	run      func(context.Context) error
}

// New returns an App on an open database without running Setup. Main uses it, and so
// do tests and tools that wire modules themselves.
func New(cfg Config, db *sql.DB) *App {
	return &App{cfg: cfg, db: db, log: slog.Default()}
}

// Name returns the app's name.
func (a *App) Name() string { return a.cfg.Name }

// DB returns the database pool. It is nil while help lists commands, so modules must
// not query the database while they are constructed.
func (a *App) DB() *sql.DB { return a.db }

// SetLogger replaces the logger, which by default writes JSON to standard error.
func (a *App) SetLogger(l *slog.Logger) { a.log = l }

// Log returns the app's logger.
func (a *App) Log() *slog.Logger { return a.log }

// Command registers an operator command.
func (a *App) Command(cmd Command) {
	a.commands = append(a.commands, cmd)
}

// Every runs job every interval while the app serves. The first run is a minute after
// startup, or after interval if that is shorter. A job that may not run twice at once
// across replicas must take its own lock.
func (a *App) Every(name string, interval time.Duration, run func(context.Context) error) {
	a.jobs = append(a.jobs, job{name: name, interval: interval, run: run})
}

// Serve sets the server the serve command runs.
func (a *App) Serve(s web.Server) {
	a.server = &s
}

// Require returns an error unless the app's Config lists m. Module constructors call
// it, so enabling a module without its migrations fails at startup.
func (a *App) Require(m Migrations) error {
	if slices.ContainsFunc(a.cfg.Migrations, func(x Migrations) bool { return x.Namespace == m.Namespace }) {
		return nil
	}
	return fmt.Errorf("the %q migrations are missing: add them to app.Config.Migrations before the app's own", m.Namespace)
}

// Main runs the command named by the program's arguments and exits.
func Main(cfg Config) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, cfg, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, cfg Config, args []string, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return 1
	}
	if cfg.Name == "" || cfg.Setup == nil {
		return fail(errors.New("app.Config needs a Name and a Setup function"))
	}
	if len(args) == 0 {
		help(ctx, cfg, stdout, logger)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		help(ctx, cfg, stdout, logger)
		return 0
	case "version":
		version(stdout)
		return 0
	case "migrate":
		if err := migrate(ctx, cfg, args[1:], stdout, logger); err != nil {
			return fail(err)
		}
		return 0
	}

	db, err := postgres.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	a := New(cfg, db)
	a.log = logger
	if err := cfg.Setup(ctx, a); err != nil {
		return fail(err)
	}
	if args[0] == "serve" && len(args) == 1 {
		if err := a.serve(ctx); err != nil {
			return fail(err)
		}
		return 0
	}
	cmd, rest, ok := a.find(args)
	if !ok {
		fmt.Fprintf(stderr, "%s: unknown command %q; run %s help\n", cfg.Name, strings.Join(args, " "), cfg.Name)
		return 2
	}
	if err := cmd.Run(ctx, rest); err != nil {
		return fail(err)
	}
	return 0
}

// find returns the registered command with the longest name matching args.
func (a *App) find(args []string) (Command, []string, bool) {
	var best Command
	bestLen := 0
	for _, cmd := range a.commands {
		words := strings.Fields(cmd.Name)
		if len(words) > bestLen && len(words) <= len(args) && slices.Equal(words, args[:len(words)]) {
			best, bestLen = cmd, len(words)
		}
	}
	return best, args[bestLen:], bestLen > 0
}

func (cfg Config) sets() []postgres.Set {
	sets := make([]postgres.Set, len(cfg.Migrations))
	for i, m := range cfg.Migrations {
		sets[i] = postgres.Set{Namespace: m.Namespace, Files: m.Files}
	}
	return sets
}

func migrate(ctx context.Context, cfg Config, args []string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "status") {
		return errors.New("usage: migrate [status]")
	}
	db, err := postgres.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if len(args) == 0 {
		return postgres.Migrate(ctx, db, cfg.sets(), logger)
	}
	status, err := postgres.Report(ctx, db, cfg.sets())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tAPPLIED\tPENDING\tNEWER")
	for _, s := range status {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", s.Namespace, len(s.Applied), list(s.Pending), list(s.Newer))
	}
	return w.Flush()
}

func list[T any](items []T) string {
	if len(items) == 0 {
		return "-"
	}
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = fmt.Sprint(item)
	}
	return strings.Join(parts, ",")
}

func (a *App) serve(ctx context.Context) error {
	if a.server == nil {
		return errors.New("setup did not call Serve")
	}
	checker, err := postgres.NewChecker(a.cfg.sets())
	if err != nil {
		return err
	}
	if err := checker.Check(ctx, a.db); err != nil {
		a.log.Warn("the database schema is not ready; run the migrate command", "error", err)
	}
	ready := func(ctx context.Context) error {
		if err := a.db.PingContext(ctx); err != nil {
			return err
		}
		return checker.Check(ctx, a.db)
	}

	jobCtx, stopJobs := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, j := range a.jobs {
		wg.Go(func() { a.runJob(jobCtx, j) })
	}
	defer func() {
		stopJobs()
		wg.Wait()
	}()

	s := *a.server
	if s.Logger == nil {
		s.Logger = a.log
	}
	return web.Serve(ctx, s, ready)
}

func (a *App) runJob(ctx context.Context, j job) {
	timer := time.NewTimer(min(time.Minute, j.interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := j.run(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("background job failed", "job", j.name, "error", err)
		}
		timer.Reset(j.interval)
	}
}

func help(ctx context.Context, cfg Config, w io.Writer, logger *slog.Logger) {
	fmt.Fprintf(w, "Usage: %s <command> [arguments]\n\nCommands:\n", cfg.Name)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  serve\tServe the app until interrupted.")
	fmt.Fprintln(tw, "  migrate\tApply pending database migrations. Needs only the database URL.")
	fmt.Fprintln(tw, "  migrate status\tShow applied and pending migrations.")
	fmt.Fprintln(tw, "  version\tShow the build's versions.")

	// Listing operator commands needs Setup, which needs the app's configuration but
	// not the database.
	a := New(cfg, nil)
	a.log = logger
	if err := cfg.Setup(ctx, a); err != nil {
		tw.Flush()
		fmt.Fprintf(w, "\nOperator commands are listed once the configuration is complete: %v\n", err)
		return
	}
	for _, cmd := range a.commands {
		fmt.Fprintf(tw, "  %s %s\t%s\n", cmd.Name, cmd.Usage, cmd.Help)
	}
	tw.Flush()
}

func version(w io.Writer) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Fprintln(w, "unknown")
		return
	}
	fmt.Fprintf(w, "%s %s\n", info.Main.Path, info.Main.Version)
	for _, dep := range info.Deps {
		if dep.Path == "github.com/coolapso/c4stack" {
			fmt.Fprintf(w, "%s %s\n", dep.Path, dep.Version)
		}
	}
}
