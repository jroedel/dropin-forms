// Command dropin-forms serves the embeddable form surface and the management
// app, from one binary on two listeners.
//
// It takes no subcommands. Everything operational -- reading submissions,
// exporting a CSV, authoring a form -- is in the management UI, because
// anything that needs an SSH session to do routinely is something that stops
// being done.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/logger"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
	"github.com/jroedel/dropin-forms/foundation/web"
)

func main() {
	if err := run(); err != nil {
		// Before the logger exists, and for anything that kills the process,
		// stderr is the only place a person will look.
		fmt.Fprintf(os.Stderr, "dropin-forms: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.toml", "path to the configuration file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	logFile, shouldClose, err := openLog(cfg.Log.File)
	if err != nil {
		return err
	}
	if shouldClose {
		defer logFile.Close()
	}

	level, err := logger.Level(cfg.Log.Level)
	if err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}

	log := logger.New(logFile, level)

	// SIGINT as well as SIGTERM: the supervisor on the server stops the
	// process with kill -INT, so treating only SIGTERM as a request to stop
	// would turn every restart into a hard kill after the grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqldb.Open(cfg.DB.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := sqldb.Init(ctx, db); err != nil {
		return err
	}

	// The schema is checked once at startup as well as on every health
	// request. Failing here means the process never begins serving, which is
	// what should happen when a binary and a database disagree -- the deploy
	// is still one rename away from the previous one.
	expected := sqldb.Infrastructure
	if err := sqldb.CheckSchema(ctx, db, expected); err != nil {
		return fmt.Errorf("the database does not match this binary: %w", err)
	}

	mc := muxer.Config{
		Log:      log,
		DB:       db,
		Expected: expected,

		// Until a form carries its own list of embedding origins, every form
		// gets the configured one. The signature is already per request so
		// that swapping in the real lookup is a change of one function and not
		// a change of shape.
		FrameAncestors: func(*http.Request) []types.Origin {
			return cfg.Embed.allowed
		},
	}

	log.Info("starting",
		"embed", cfg.Server.EmbedAddr,
		"admin", cfg.Server.AdminAddr,
		"db", cfg.DB.Path,
		"embed_allowed_origins", len(cfg.Embed.allowed),
	)

	return web.Serve(ctx, log, cfg.Server.shutdownGraceD,
		web.Surface{Name: "embed", Addr: cfg.Server.EmbedAddr, Handler: muxer.Embed(mc)},
		web.Surface{Name: "admin", Addr: cfg.Server.AdminAddr, Handler: muxer.Admin(mc)},
	)
}
