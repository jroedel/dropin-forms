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
	"database/sql"
	"flag"
	"fmt"
	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/paydb"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/stripepay"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"log/slog"
	"os"
	"os/signal"
	"strings"
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

		// Whether money is being taken, and in which mode, from the key's own
		// prefix. Never the key. This is the line somebody reads at the worst
		// possible moment, and "test" where they expected "LIVE" is the answer
		// they need to see without having to look anywhere else.
		fmt.Printf("  payments       %s\n", stripeMode(cfg))
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
	if err := paydb.Init(ctx, db); err != nil {
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
		paydb.Expected,
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

	payments, howPay, err := newPayments(log, cfg, db, submissions)
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

			// Nil when Stripe is not configured, which embedapp handles as a
			// form that stores its orders and shows no way to pay. An
			// interface holding a typed nil would not be nil, so this is
			// assigned below rather than here.
		},
	}

	// Assigned rather than set in the literal above, because `Payments:
	// payments` with a nil *paybus.Business would store a non-nil interface
	// holding a nil pointer -- and every nil check downstream would pass while
	// the first method call panicked. This is the one Go trap in the wiring
	// and it is worth the four lines.
	if payments != nil {
		mc.Payments = payments
		mc.Embed.Payments = payments
	}

	log.Info("starting",
		"embed", cfg.Server.EmbedAddr,
		"admin", cfg.Server.AdminAddr,
		"admin_base_url", cfg.Server.AdminBaseURL,
		"db", cfg.DB.Path,
		"embed_allowed_origins", len(cfg.Embed.allowed),
		"mail", howMail,
		"payments", howPay,

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

	// Two listeners, not three. The webhook lives on the embed one, mounted
	// outside its origin gate -- see muxer.Embed for why that is the whole of
	// what it needs, and why it is not enough for it merely to pass the gate.
	return web.Serve(ctx, log, cfg.Server.shutdownGraceD,
		web.Surface{Name: "embed", Addr: cfg.Server.EmbedAddr, Handler: embed},
		web.Surface{Name: "admin", Addr: cfg.Server.AdminAddr, Handler: admin},
	)
}

// newPayments builds the payment domain, and returns a word for the log saying
// whether there is one.
//
// Nil and no error when Stripe is not configured. That is the same shape
// newSender uses for a missing mail relay and for the same reason: a developer
// has to be able to run this without an account, and an operator who has
// forgotten to configure one should find out from a line in the log rather
// than from somebody reporting that nothing can be paid for.
//
// The difference from mail is that there is no recorder here. A fake payment
// gateway that pretends to take money is a much worse thing to leave switched
// on by accident than a mailer that prints to a log.
func newPayments(log *slog.Logger, cfg config, db *sql.DB, submissions *submissionbus.Business) (*paybus.Business, string, error) {
	if cfg.Stripe.SecretKey == "" {
		log.Warn("Stripe is not configured, so nothing can be paid for",
			"consequence", "a form that sells stores its orders as pending and shows no payment button")

		return nil, "off", nil
	}

	gateway, err := stripepay.New(cfg.Stripe.SecretKey, cfg.Stripe.WebhookSecret)
	if err != nil {
		return nil, "", err
	}

	// Which mode, from the key's own prefix, because this is the one line in
	// the log that answers "are we taking real money" -- and it is the
	// question somebody asks at the worst moment. Never the key itself.
	mode := "test"
	if strings.HasPrefix(cfg.Stripe.SecretKey, "sk_live_") {
		mode = "LIVE"
	}

	return paybus.NewBusiness(log, gateway, paydb.NewStore(db), submissions), mode, nil
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

// stripeMode says whether payments are on, and whether they are real.
//
// From the key's prefix rather than from a separate setting, because two
// places to say "this is live" is one place to get it wrong. Never returns any
// part of the key itself: this goes into a deploy log.
func stripeMode(cfg config) string {
	switch {
	case cfg.Stripe.SecretKey == "":
		return "off: a form that sells will store orders as pending"
	case strings.HasPrefix(cfg.Stripe.SecretKey, "sk_live_"):
		return "LIVE: real cards will be charged"
	default:
		return "test mode"
	}
}

// either is a ternary for a log line, so the -check output above stays a list
// of one-line facts rather than a run of if statements.
func either(cond bool, yes, no string) string {
	if cond {
		return yes
	}

	return no
}
