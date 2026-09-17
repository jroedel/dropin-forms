package main

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
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
		EmbedAddr      string `toml:"embed_addr"`
		AdminAddr      string `toml:"admin_addr"`
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
	} `toml:"embed"`

	DB struct {
		Path string `toml:"path"`
	} `toml:"db"`

	Log struct {
		Level string `toml:"level"`
		File  string `toml:"file"`
	} `toml:"log"`
}

// loadConfig reads and validates the file.
//
// Every failure here is a startup failure with a sentence naming the key, and
// that is the point: a service that starts with a half-understood
// configuration is a service that answers wrongly at three in the morning. An
// unparseable origin in particular must never become a header -- it is the one
// value in this file that a form owner types and that ends up in the CSP of a
// page leading to a payment.
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
