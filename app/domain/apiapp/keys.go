package apiapp

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/apikey/apikeybus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's template directory, for page.NewRenderer.
var Templates = templates

// Keys is the key domain, as the page that manages one account's keys needs
// it.
type Keys interface {
	Create(ctx context.Context, now time.Time, userID types.ID, label string) (apikeybus.Key, string, error)
	ForUser(ctx context.Context, userID types.ID) ([]apikeybus.Key, error)
	Revoke(ctx context.Context, now time.Time, userID, id types.ID) error
}

// KeysConfig is what the key page needs.
type KeysConfig struct {
	Log    *slog.Logger
	Keys   Keys
	Render *page.Renderer

	// BaseURL is the admin surface's own address, which is where the API is,
	// for the example on the page.
	BaseURL string
}

type keysApp struct {
	cfg KeysConfig
}

// KeyRoutes mounts the page that makes and revokes the signed-in account's
// own keys. guard is Require.
//
// On the browser's chain, behind a session, and nowhere a key can reach: a
// key that could make a key would turn one leak into a permanent one. Behind
// Require and nothing else, because a key is somebody's own -- whatever their
// grants, anybody with an account may make one, and it reaches exactly what
// they do.
func KeyRoutes(mux *http.ServeMux, cfg KeysConfig, guard func(http.Handler) http.Handler) {
	k := keysApp{cfg: cfg}

	mux.Handle("GET /account/keys", guard(http.HandlerFunc(k.page)))
	mux.Handle("POST /account/keys", guard(http.HandlerFunc(k.create)))
	mux.Handle("POST /account/keys/{id}/revoke", guard(http.HandlerFunc(k.revoke)))
}

type keysView struct {
	Email   string
	BaseURL string

	// Secret is a key just made, shown this once.
	Secret string

	Keys []keyRow
}

type keyRow struct {
	ID       string
	Label    string
	Made     string
	LastUsed string
	Ends     string
	Live     bool

	// Revoked tells a key somebody revoked from one that ran out, which is
	// the difference between "I did that" and "Claude needs connecting
	// again".
	Revoked bool
}

func (k keysApp) page(w http.ResponseWriter, r *http.Request) {
	k.render(w, r, "")
}

// create makes a key and shows it on the page it answers with, which is the
// only time it is ever shown -- a page rather than a redirect, for the reason
// feedapp.create gives.
func (k keysApp) create(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		k.oops(w, r, "making an api key was reached with no account", nil)

		return
	}

	_, secret, err := k.cfg.Keys.Create(r.Context(), time.Now(), me.ID, r.PostFormValue("label"))
	if err != nil {
		k.oops(w, r, "an api key could not be made", err)

		return
	}

	k.render(w, r, secret)
}

func (k keysApp) revoke(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		k.oops(w, r, "revoking an api key was reached with no account", nil)

		return
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "there is no such key.", http.StatusNotFound)

		return
	}

	// The account goes down to the store's condition, so a key somebody else
	// owns is left alone rather than revoked by whoever edited the URL.
	if err := k.cfg.Keys.Revoke(r.Context(), time.Now(), me.ID, id); err != nil {
		k.oops(w, r, "an api key could not be revoked", err)

		return
	}

	http.Redirect(w, r, "/account/keys", http.StatusSeeOther)
}

func (k keysApp) render(w http.ResponseWriter, r *http.Request, secret string) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		k.oops(w, r, "the api key page was reached with no account", nil)

		return
	}

	keys, err := k.cfg.Keys.ForUser(r.Context(), me.ID)
	if err != nil {
		k.oops(w, r, "the api keys could not be read", err)

		return
	}

	view := keysView{Email: me.Email.String(), BaseURL: k.cfg.BaseURL, Secret: secret}

	now := time.Now()

	for _, key := range keys {
		row := keyRow{
			ID:       key.ID.String(),
			Label:    key.Label,
			Made:     key.CreatedAt.Local().Format("2 January 2006"),
			LastUsed: "never",
			Ends:     "never",
			Live:     key.Live(now),
			Revoked:  !key.RevokedAt.IsZero(),
		}

		if !key.ExpiresAt.IsZero() {
			row.Ends = key.ExpiresAt.Local().Format("2 January 2006")
		}

		if !key.LastUsedAt.IsZero() {
			row.LastUsed = key.LastUsedAt.Local().Format("2 January 2006, 15:04")
		}

		view.Keys = append(view.Keys, row)
	}

	k.cfg.Render.Render(w, r, http.StatusOK, "api-keys", view)
}

func (k keysApp) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	k.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}
