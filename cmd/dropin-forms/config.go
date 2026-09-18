package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// config is the whole of what this binary reads from disk.
//
// TOML because there is no parser in the standard library and .gitignore
// already commits to the shape: config.toml is ignored, config.example.toml is
// tracked and is the documentation. The real file is written onto the server by
// `make secrets-install` and never lives in git.
type config struct {
	Server struct {
		EmbedAddr string `toml:"embed_addr"`
		AdminAddr string `toml:"admin_addr"`
		// AdminBaseURL is this service's own admin origin, used to build the
		// link that goes in a sign-in email. Not derived from the request: a
		// link built from a Host header is one an attacker can aim at their
		// own host with a single request, and whoever receives the mail
		// cannot tell.
		AdminBaseURL string `toml:"admin_base_url"`

		ShutdownGrace  string `toml:"shutdown_grace"`
		shutdownGraceD time.Duration
	} `toml:"server"`

	Embed struct {
		// AllowedOrigins is the fallback answer to "who may frame a form",
		// used until each form carries its own list. It is a list rather than
		// a single value because a site can legitimately need more than one
		// origin -- frame-ancestors checks every ancestor, not just the
		// parent, and Squarespace nests pages inside editor chrome.
		AllowedOrigins []string `toml:"allowed_origins"`
		allowed        []types.Origin

		// GrantSigningKey signs the submission grants a form page hands out
		// and a submission hands back. Required: without it no form can be
		// rendered, since every page carries a grant.
		//
		// Changing it invalidates every grant in flight, which is a handful
		// of re-rendered pages and no lost data -- so it is rotatable, and
		// there is no reason to keep a compromised one.
		GrantSigningKey string `toml:"grant_signing_key"`
		grantKey        formbus.GrantKey

		// TrustProxy says whether X-Forwarded-For carries the visitor's
		// address.
		//
		// Configured rather than sniffed, and false by default. This process
		// listens on the loopback behind Apache, so the socket's own address
		// is always 127.0.0.1 and the visitor's is only in a header -- and a
		// header is exactly as trustworthy as whatever put it there. Trusting
		// one with nothing in front means letting a stranger choose the
		// address that goes to Stripe's fraud checks, which is worse than
		// having no address at all.
		TrustProxy bool `toml:"trust_proxy"`
	} `toml:"embed"`

	DB struct {
		Path string `toml:"path"`
	} `toml:"db"`

	Log struct {
		Level string `toml:"level"`
		File  string `toml:"file"`
	} `toml:"log"`

	Auth struct {
		// BootstrapSecret produces one session without sending mail, exactly
		// once. Empty leaves those routes unmounted, which is the right answer
		// once the service has accounts. See userbus.Business.Bootstrap for
		// why it exists at all.
		BootstrapSecret string `toml:"bootstrap_secret"`
	} `toml:"auth"`

	Stripe struct {
		// SecretKey is the Stripe API key, sk_test_… or sk_live_…. Empty
		// switches the payment step off entirely: forms still validate and
		// store, and a form that sells shows no way to pay. That is a
		// deliberate degradation rather than a startup failure, so that this
		// service runs on a laptop and in a test without a Stripe account.
		SecretKey string `toml:"secret_key"`

		// WebhookSecret is the signing secret for the endpoint, whsec_….
		// It is a *different credential* from the key above and is per
		// endpoint: the one Stripe shows when you create the webhook, or the
		// one `stripe listen` prints. Confusing the two produces a signature
		// that never verifies, which is why they are checked apart below.
		WebhookSecret string `toml:"webhook_secret"`
	} `toml:"stripe"`

	Mail struct {
		Host     string `toml:"host"`
		Port     int    `toml:"port"`
		User     string `toml:"user"`
		Password string `toml:"password"`
		From     string `toml:"from"`
		FromName string `toml:"from_name"`
	} `toml:"mail"`
}

// loadConfig reads and validates the file.
//
// Every failure here is a startup failure with a sentence naming the key, and
// that is the point: a service that starts with a half-understood
// configuration is a service that answers wrongly at three in the morning. An
// unparseable origin in particular must never become a header -- it is the one
// value in this file that a form owner types and that ends up in the CSP of a
// page leading to a payment.
// bootstrapMinLen is the shortest bootstrap secret this service will accept.
// `openssl rand -base64 32` and `scripts/secrets keygen` both produce more
// than this.
const bootstrapMinLen = 32

func loadConfig(path string) (config, error) {
	var cfg config

	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	// An unrecognised key is almost always a typo, and a typo in a config file
	// is silent by nature: the value you meant to set keeps its default.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return config{}, fmt.Errorf("%s has settings this version does not understand: %v", path, undecoded)
	}

	if cfg.Server.EmbedAddr == "" || cfg.Server.AdminAddr == "" {
		return config{}, fmt.Errorf("%s needs both server.embed_addr and server.admin_addr; they are separate listeners on purpose", path)
	}
	if cfg.Server.EmbedAddr == cfg.Server.AdminAddr {
		return config{}, fmt.Errorf("%s gives server.embed_addr and server.admin_addr the same address; the two surfaces are told apart by which socket accepted the request, so they must differ", path)
	}

	if cfg.DB.Path == "" {
		return config{}, fmt.Errorf("%s needs db.path", path)
	}

	grace := 15 * time.Second
	if s := cfg.Server.ShutdownGrace; s != "" {
		grace, err = time.ParseDuration(s)
		if err != nil {
			return config{}, fmt.Errorf("%s has an unreadable server.shutdown_grace %q: write it as 15s", path, s)
		}
	}
	cfg.Server.shutdownGraceD = grace

	for _, s := range cfg.Embed.AllowedOrigins {
		o, err := types.ParseOrigin(s)
		if err != nil {
			return config{}, fmt.Errorf("%s has a bad entry in embed.allowed_origins: %w", path, err)
		}
		cfg.Embed.allowed = append(cfg.Embed.allowed, o)
	}

	// The admin origin goes into an email as a link somebody clicks, so it is
	// held to the same standard as an origin somebody may frame us from:
	// https, a host, and nothing else. A trailing slash is trimmed rather than
	// refused, since the routes appended to it all begin with one and the
	// result would be a double slash -- a path that works but looks wrong in
	// the one place a person is deciding whether to trust a link.
	switch base := strings.TrimSuffix(cfg.Server.AdminBaseURL, "/"); {
	case base == "":
		return config{}, fmt.Errorf("%s needs server.admin_base_url, which is the address that goes into a sign-in email", path)
	default:
		o, err := types.ParseOrigin(base)
		if err != nil {
			return config{}, fmt.Errorf("%s has a bad server.admin_base_url: %w", path, err)
		}

		// Re-serialised from the parsed value rather than kept as written,
		// for the same reason the frame-ancestors list is.
		cfg.Server.AdminBaseURL = o.String()
	}

	// Every form page carries a grant, so a missing key is not a degraded
	// service, it is a service with no forms. Refused at startup, where the
	// deploy is still one rename away from the previous release.
	switch key := cfg.Embed.GrantSigningKey; {
	case key == "":
		return config{}, fmt.Errorf("%s needs embed.grant_signing_key; every form page carries a submission grant signed with it. Generate one with: openssl rand -base64 48", path)
	default:
		k, err := formbus.ParseGrantKey(key)
		if err != nil {
			return config{}, fmt.Errorf("%s has an unusable embed.grant_signing_key: %w. Generate one with: openssl rand -base64 48", path, err)
		}

		cfg.Embed.grantKey = k
	}

	// A short bootstrap secret is worse than none: it is a guessable route to
	// a founding session that sends no mail and so leaves no trace anywhere
	// but our own log. Refused at startup rather than accepted and warned
	// about.
	if secret := cfg.Auth.BootstrapSecret; secret != "" && len(secret) < bootstrapMinLen {
		return config{}, fmt.Errorf("%s has an auth.bootstrap_secret of %d characters; use at least %d, or leave it empty to switch that route off", path, len(secret), bootstrapMinLen)
	}

	// The two Stripe settings stand or fall together, and a half-configured
	// payment path is the one shape worth refusing at startup: a key with no
	// webhook secret means money collected and no submission ever marked paid,
	// and nothing anywhere would say so. The office would find out by
	// comparing a bank statement against a list of pending orders.
	switch st := cfg.Stripe; {
	case st.SecretKey == "" && st.WebhookSecret == "":
		// Payments off. A form that sells stores its orders as pending and
		// shows no way to pay, which is what this service did before the
		// payment step existed.

	case st.SecretKey == "":
		return config{}, fmt.Errorf("%s configures a Stripe webhook secret but no stripe.secret_key; nothing could create a payment", path)

	case st.WebhookSecret == "":
		return config{}, fmt.Errorf("%s has stripe.secret_key but no stripe.webhook_secret; without it nothing could tell a real payment notification from a forged one, and no order would ever be marked paid. It is the whsec_... value Stripe shows when you create the endpoint", path)

	case !strings.HasPrefix(st.SecretKey, "sk_"):
		// Named rather than guessed at. A publishable key (pk_...) in this
		// slot is a plausible mistake and produces an authentication failure
		// from Stripe on the first order somebody places.
		return config{}, fmt.Errorf("%s has a stripe.secret_key that does not begin with sk_; a publishable pk_ key cannot create a payment", path)

	case !strings.HasPrefix(st.WebhookSecret, "whsec_"):
		return config{}, fmt.Errorf("%s has a stripe.webhook_secret that does not begin with whsec_; that is the endpoint's signing secret, not the API key", path)
	}

	if cfg.Mail.Host != "" {
		switch {
		case cfg.Mail.Port <= 0 || cfg.Mail.Port > 65535:
			return config{}, fmt.Errorf("%s has mail.host but mail.port is %d; 587 is the usual one", path, cfg.Mail.Port)
		case cfg.Mail.From == "":
			return config{}, fmt.Errorf("%s has mail.host but no mail.from; that address has to be one the relay will send as", path)
		}
	}

	return cfg, nil
}

// openLog returns where log lines go, and whether the caller has to close it.
//
// An empty log.file means stderr, which is what a developer wants and what the
// cron-driven supervisor on the server redirects into a file anyway.
func openLog(path string) (*os.File, bool, error) {
	if path == "" {
		return os.Stderr, false, nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("opening the log file %s: %w", path, err)
	}

	return f, true, nil
}
