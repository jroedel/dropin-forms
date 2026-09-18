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
	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/forms"
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

	// -check exists for the deploy, not for a person.
	//
	// A binary and its configuration file travel by different routes: the
	// binary through CD, the file through `make secrets-install` from a
	// laptop, because the Stripe and SMTP credentials in it deliberately
	// never pass through GitHub. So the two can disagree, and the way that
	// disagreement used to present was a service that ran happily until
	// something restarted it and then would not start at all -- loadConfig
	// refuses a key it does not recognise, which is right, and silent until
	// the worst moment.
	//
	// With this flag the deploy can ask the incoming binary whether it
	// accepts the config already on the server, while the outgoing one is
	// still running and nothing has been swapped.
	check := flag.Bool("check", false, "load the configuration, report whether it is usable, and exit")

	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	if *check {
		// Deliberately terse and free of secrets: this runs over SSH and its
		// output lands in a deploy log. It names what it read, never a value
		// from it.
		fmt.Printf("config ok: %s\n", *configPath)
		fmt.Printf("  embed          %s\n", cfg.Server.EmbedAddr)
		fmt.Printf("  admin          %s\n", cfg.Server.AdminAddr)
		fmt.Printf("  admin base url %s\n", cfg.Server.AdminBaseURL)
		fmt.Printf("  database       %s\n", cfg.DB.Path)
		fmt.Printf("  embed origins  %d\n", len(cfg.Embed.allowed))
		fmt.Printf("  mail relay     %s\n", either(cfg.Mail.Host != "", "configured", "none: mail will not be sent"))
		fmt.Printf("  bootstrap      %s\n", either(cfg.Auth.BootstrapSecret != "", "route mounted", "route not mounted"))

		return nil
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
	if err := userdb.Init(ctx, db); err != nil {
		return err
	}

	// After userdb, because a grant row references an account row. SQLite
	// tolerates the other order for a CREATE TABLE, and relying on that would
	// be relying on the thing being tolerated.
	if err := accessdb.Init(ctx, db); err != nil {
		return err
	}
	if err := submissiondb.Init(ctx, db); err != nil {
		return err
	}

	// The schema is checked once at startup as well as on every health
	// request. Failing here means the process never begins serving, which is
	// what should happen when a binary and a database disagree -- the deploy
	// is still one rename away from the previous one.
	//
	// Each store contributes what it will read, so a table one of them needs
	// and nothing created is caught here rather than on the first request
	// that touches it.
	expected := sqldb.Expected{}
	for _, part := range []sqldb.Expected{
		sqldb.Infrastructure, userdb.Expected, accessdb.Expected, submissiondb.Expected,
	} {
		for table, columns := range part {
			if _, clash := expected[table]; clash {
				return fmt.Errorf("two stores both claim the table %s", table)
			}

			expected[table] = columns
		}
	}

	if err := sqldb.CheckSchema(ctx, db, expected); err != nil {
		return fmt.Errorf("the database does not match this binary: %w", err)
	}

	users := userbus.NewBusiness(log, userdb.NewStore(db))
	access := accessbus.NewBusiness(log, accessdb.NewStore(db))
	submissions := submissionbus.NewBusiness(log, submissiondb.NewStore(db))

	// The definitions are embedded in the binary, so this cannot fail for a
	// missing file -- only for a definition that does not pass Check, which is
	// a build-time mistake caught here at the last possible moment rather
	// than on the first request for that form.
	definitions, err := formtoml.Load(forms.FS)
	if err != nil {
		return fmt.Errorf("the form definitions are not usable: %w", err)
	}

	sender, howMail, err := newSender(log, cfg)
	if err != nil {
		return err
	}

	adminPages, err := page.NewRenderer(log, page.AdminChrome(), authapp.Templates, submissionapp.Templates)
	if err != nil {
		return err
	}

	embedPages, err := page.NewRenderer(log, page.EmbedChrome(), embedapp.Templates)
	if err != nil {
		return err
	}

	mc := muxer.Config{
		Log:      log,
		DB:       db,
		Expected: expected,

		Users:        users,
		Access:       access,
		Submissions:  submissions,
		Forms:        definitions,
		Mail:         sender,
		Render:       adminPages,
		AdminBaseURL: cfg.Server.AdminBaseURL,
		Bootstrap:    cfg.Auth.BootstrapSecret,

		// Each form's own list, which is what makes frame-ancestors a
		// property of the form rather than of the installation. The
		// configured list is the fallback for a definition that names none.
		FrameAncestors: page.FormFrameAncestors(definitions, cfg.Embed.allowed),

		Embed: embedapp.Config{
			Log:         log,
			Forms:       definitions,
			Submissions: submissions,
			Render:      embedPages,
			GrantKey:    cfg.Embed.grantKey,
			TrustProxy:  cfg.Embed.TrustProxy,
		},
	}

	log.Info("starting",
		"embed", cfg.Server.EmbedAddr,
		"admin", cfg.Server.AdminAddr,
		"admin_base_url", cfg.Server.AdminBaseURL,
		"db", cfg.DB.Path,
		"embed_allowed_origins", len(cfg.Embed.allowed),
		"mail", howMail,

		// Whether the route exists, never the secret. Worth logging because
		// a bootstrap route left mounted after the first sign-in is something
		// somebody should notice and remove.
		"bootstrap_route", cfg.Auth.BootstrapSecret != "",
	)

	admin, err := muxer.Admin(mc)
	if err != nil {
		return err
	}

	embed, err := muxer.Embed(mc)
	if err != nil {
		return err
	}

	return web.Serve(ctx, log, cfg.Server.shutdownGraceD,
		web.Surface{Name: "embed", Addr: cfg.Server.EmbedAddr, Handler: embed},
		web.Surface{Name: "admin", Addr: cfg.Server.AdminAddr, Handler: admin},
	)
}

// newSender builds the outbound mailer, and returns a word for the log saying
// which kind it is.
//
// With no relay configured it records messages instead of sending them, and
// says so loudly rather than failing to start. That combination is deliberate:
// this service's only ordinary way in is an emailed link, so a developer
// running it locally needs it to come up without a relay -- and an operator
// who has forgotten to configure one needs to find out from a line in the log
// rather than from somebody reporting that no mail arrives.
func newSender(log *slog.Logger, cfg config) (mail.Sender, string, error) {
	if cfg.Mail.Host == "" {
		log.Warn("no mail relay is configured, so no mail will be sent",
			"consequence", "sign-in links cannot arrive; use auth.bootstrap_secret or a backup code")

		return &mail.Recorder{}, "recorded, not sent", nil
	}

	sender, err := mail.NewSMTP(mail.Config{
		Host:     cfg.Mail.Host,
		Port:     cfg.Mail.Port,
		User:     cfg.Mail.User,
		Password: cfg.Mail.Password,
		From:     cfg.Mail.From,
		FromName: cfg.Mail.FromName,
	})
	if err != nil {
		return nil, "", err
	}

	return sender, fmt.Sprintf("%s:%d as %s", cfg.Mail.Host, cfg.Mail.Port, cfg.Mail.From), nil
}

// either is a ternary for a log line, so the -check output above stays a list
// of one-line facts rather than a run of if statements.
func either(cond bool, yes, no string) string {
	if cond {
		return yes
	}

	return no
}
