package page

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/foundation/web"
)

// assets holds both surfaces' chrome: an outer layout and a stylesheet each.
//
// Two of them, because the two surfaces are not the same page with different
// contents. The admin app is a page somebody navigates, with a masthead and a
// footer; a form is a fragment that lives inside an iframe on somebody else's
// website, where a masthead would be a second heading under theirs and a
// footer would be furniture inside a box the height of its contents.
//
//go:embed assets
var assets embed.FS

// Chrome is one surface's shared layout and stylesheet: a directory holding
// *.html templates that define "base", and an app.css.
type Chrome struct {
	fs   fs.FS
	name string
}

// AdminChrome is the management surface's layout: masthead, footer, the lot.
func AdminChrome() Chrome { return chrome("admin") }

// EmbedChrome is the embedded form's layout. No masthead and no footer -- see
// the comment on assets.
func EmbedChrome() Chrome { return chrome("embed") }

func chrome(name string) Chrome {
	// fs.Sub on an embedded directory cannot fail for a name that is a valid
	// path, and both names here are literals, so a failure is a programming
	// error rather than a condition. It is still not ignored: the zero fs
	// would produce "there is no layout" from NewRenderer, which is a worse
	// message than the truth.
	sub, err := fs.Sub(assets, "assets/"+name)
	if err != nil {
		panic("page: the " + name + " chrome is missing from the binary: " + err.Error())
	}

	return Chrome{fs: sub, name: name}
}

// Renderer turns a named template into a response.
//
// Templates are parsed once, at startup, from an embedded filesystem. A parse
// error is therefore a startup failure rather than a 500 in front of somebody
// -- and the binary is a deployable artefact on its own, with no directory of
// templates to keep in step with it.
//
// # One template set per page, not one set for everything
//
// Every page template defines "content", so parsing them all into a single
// set would have them overwrite each other and leave whichever was parsed last
// -- silently, with no error, and only visible as the wrong page being served.
// So the shared layout is parsed once and then cloned per page, which is what
// makes {{block "content"}} in the layout mean a different thing for each one.
type Renderer struct {
	log   *slog.Logger
	pages map[string]*template.Template

	// The stylesheet is served under a path containing a hash of its content,
	// so it can be cached forever and still change the moment it is edited.
	// The alternative -- a fixed path with a short max-age -- is a deploy
	// where the markup is new and the styling is whatever the browser kept.
	css     []byte
	cssPath string
	cssETag string

	// A surface may also ship one script, and only the embed surface does.
	// It is optional in the same way the layout is not: a chrome directory
	// with no app.js produces a renderer whose pages reference none, and the
	// admin surface's Content-Security-Policy has no script-src to run one
	// under anyway.
	js     []byte
	jsPath string
	jsETag string
}

// shell is what every template is executed against.
//
// The page's own data is nested under Data rather than merged in, so that a
// field a handler adds can never shadow something the layout needs. The layout
// reads .Stylesheet; the page reads . after the layout hands it .Data.
type shell struct {
	Stylesheet string

	// Script is the enhancement script's path, or empty. Empty is the
	// ordinary case for the admin surface, which has no scripts at all and a
	// Content-Security-Policy with no script-src to run one under.
	Script string

	Data any
}

// NewRenderer parses one surface's shared layout together with each of one
// app's templates.
func NewRenderer(log *slog.Logger, ch Chrome, own fs.FS) (*Renderer, error) {
	if ch.fs == nil {
		return nil, errors.New("a renderer needs a chrome; use AdminChrome or EmbedChrome")
	}

	base, err := template.New("base").ParseFS(ch.fs, "*.html")
	if err != nil {
		return nil, fmt.Errorf("the %s layout could not be read: %w", ch.name, err)
	}

	names, err := fs.Glob(own, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("the page templates could not be listed: %w", err)
	}

	if len(names) == 0 {
		return nil, errors.New("there are no page templates to read")
	}

	pages := make(map[string]*template.Template, len(names))

	for _, name := range names {
		// Cloned per page, so each page's "content" replaces the layout's
		// empty block without touching any other page's.
		set, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("the shared layout could not be copied: %w", err)
		}

		if set, err = set.ParseFS(own, name); err != nil {
			return nil, fmt.Errorf("%s could not be read: %w", name, err)
		}

		page := strings.TrimSuffix(path.Base(name), ".html")

		if _, taken := pages[page]; taken {
			return nil, fmt.Errorf("there is more than one %s template", page)
		}

		pages[page] = set
	}

	css, err := fs.ReadFile(ch.fs, "app.css")
	if err != nil {
		return nil, fmt.Errorf("the %s stylesheet could not be read: %w", ch.name, err)
	}

	sum := sha256.Sum256(css)
	digest := hex.EncodeToString(sum[:])[:12]

	// The digest is in the path, so the two surfaces' stylesheets cannot
	// collide even though both are called app.css -- and each is cacheable
	// forever while still changing the moment it is edited.
	rn := &Renderer{
		log:     log,
		pages:   pages,
		css:     css,
		cssPath: "/static/app." + digest + ".css",
		cssETag: `"` + digest + `"`,
	}

	// One optional script, found the same way. fs.ReadFile failing is read as
	// "this surface has none" rather than distinguished, because the only
	// other reason an embedded file cannot be read is a corrupt binary, and
	// that has larger symptoms than a missing script.
	if js, err := fs.ReadFile(ch.fs, "app.js"); err == nil {
		sum := sha256.Sum256(js)
		digest := hex.EncodeToString(sum[:])[:12]

		rn.js = js
		rn.jsPath = "/static/app." + digest + ".js"
		rn.jsETag = `"` + digest + `"`
	}

	return rn, nil
}

// StylesheetPath is where the stylesheet is served, including its content
// hash. Handed to the muxer so it can mount it.
func (rn *Renderer) StylesheetPath() string { return rn.cssPath }

// ScriptPath is where this surface's script is served, or empty when it has
// none. The muxer mounts it only when there is one.
func (rn *Renderer) ScriptPath() string { return rn.jsPath }

// Render writes a page.
//
// Executed into a buffer first, and the status written only once that
// succeeded. Writing the header and streaming straight to the
// ResponseWriter would mean a template error halfway down produces a
// half-rendered page under a 200, which is worse than an error page: the
// person sees something that looks like it worked.
func (rn *Renderer) Render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	set, ok := rn.pages[name]
	if !ok {
		// A handler naming a template that does not exist. Not recoverable at
		// runtime, so it is an error page and a loud line rather than a blank
		// response.
		rn.log.Error("a handler asked for a page that does not exist",
			"request_id", web.RequestIDFrom(r.Context()), "template", name)
		http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

		return
	}

	var buf bytes.Buffer

	if err := set.ExecuteTemplate(&buf, "base", shell{
		Stylesheet: rn.cssPath,
		Script:     rn.jsPath,
		Data:       data,
	}); err != nil {
		rn.log.Error("a page could not be rendered",
			"request_id", web.RequestIDFrom(r.Context()), "template", name, "error", err)

		// Plain text, because whatever is wrong with the templates may well be
		// wrong with an error template too.
		http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		// The status and most of the body are already gone, so there is
		// nothing to answer with -- only something to record.
		rn.log.Warn("a page was cut off while being sent",
			"request_id", web.RequestIDFrom(r.Context()), "template", name, "error", err)
	}
}

// Stylesheet serves the embedded stylesheet.
//
// Cacheable forever, because the path contains a hash of the content: a new
// stylesheet is a new URL, so there is no version of this file a browser can
// hold on to by mistake. That is also why the response overrides the
// surface's Cache-Control -- the admin policy sets no-store, which is right
// for a page showing somebody's answers and wrong for a file compiled into the
// binary.
func (rn *Renderer) Stylesheet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/css; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", rn.cssETag)

		// Not behind any credential, deliberately. It is a file compiled into
		// the binary rather than anybody's data, and the sign-in page needs
		// it -- put the stylesheet behind the session and the login page
		// renders unstyled.
		http.ServeContent(w, r, "app.css", startup, bytes.NewReader(rn.css))
	}
}

// Script serves this surface's script, on the same terms as the stylesheet:
// content-hashed path, cached forever, no credential in front of it.
//
// The Cache-Control here overrides the surface's own no-store, and that is the
// point of the hashed path. no-store is right for a form page carrying a
// single-use grant and wrong for a static asset that cannot change without
// changing its URL.
func (rn *Renderer) Script() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", rn.jsETag)

		http.ServeContent(w, r, "app.js", startup, bytes.NewReader(rn.js))
	}
}

// startup is the modification time reported for embedded assets. embed.FS
// records no times, and handing ServeContent a zero time makes it omit
// Last-Modified and skip conditional requests -- so the process start is used
// instead, which is honest about how long this copy has existed.
var startup = time.Now()
