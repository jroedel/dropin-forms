package page_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/sdk/page"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// assetOf fetches one of a renderer's own assets through its handler, which is
// how the surface serves it.
func assetOf(t *testing.T, rn *page.Renderer, path string, h http.HandlerFunc) string {
	t.Helper()

	if path == "" {
		t.Fatal("the renderer has no such asset")
	}

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, path, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, w.Code)
	}

	return w.Body.String()
}

// The embedded form must not follow the viewer's colour scheme.
//
// prefers-color-scheme reports the viewer's operating-system setting, not the
// design of the page the form has been dropped into -- and an iframe cannot
// look across origins to find out. On the real Squarespace page, which is
// cream whatever the visitor has chosen, honouring it rendered the form as a
// black box in the middle of the page for every visitor in dark mode.
//
// This is asserted rather than left to a comment because the obvious way to
// edit this stylesheet is to copy a block out of the admin one, which does
// have a dark mode and is right to.
func TestTheEmbeddedFormDoesNotFollowTheViewersColourScheme(t *testing.T) {
	rn, err := page.NewRenderer(discard(), page.EmbedChrome(), embedapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	css := assetOf(t, rn, rn.StylesheetPath(), rn.Stylesheet())

	// The comment at the top of the file explains the decision and mentions
	// the property by name, so the check is for the media query itself.
	if strings.Contains(css, "@media (prefers-color-scheme") {
		t.Error("the embed stylesheet has a prefers-color-scheme block; the host page's design is not the viewer's setting")
	}

	// Load-bearing, not tidiness: `light dark` lets the browser draw inputs,
	// dropdowns and spinners in dark widget colours over these light
	// backgrounds, which looks worse than either scheme on its own.
	if !strings.Contains(css, "color-scheme: light;") {
		t.Error("the embed stylesheet does not pin color-scheme to light")
	}
	if strings.Contains(css, "color-scheme: light dark") {
		t.Error("the embed stylesheet still allows dark form controls")
	}
}

// The management app is a page somebody navigates rather than a fragment on
// somebody else's site, so following the viewer's setting is right there. The
// two surfaces differ on purpose, and this says so.
func TestTheAdminSurfaceDoesFollowIt(t *testing.T) {
	rn, err := page.NewRenderer(discard(), page.AdminChrome(), authapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	css := assetOf(t, rn, rn.StylesheetPath(), rn.Stylesheet())

	if !strings.Contains(css, "@media (prefers-color-scheme: dark)") {
		t.Error("the admin stylesheet no longer follows the viewer's colour scheme")
	}
}

// The two surfaces' stylesheets are both called app.css, and only the content
// hash in the path keeps them apart. If that ever stopped being true, the two
// would collide on one route and the second registration would panic at
// startup.
func TestTheTwoSurfacesAssetPathsDiffer(t *testing.T) {
	embed, err := page.NewRenderer(discard(), page.EmbedChrome(), embedapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer(embed): %v", err)
	}

	admin, err := page.NewRenderer(discard(), page.AdminChrome(), authapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer(admin): %v", err)
	}

	if embed.StylesheetPath() == admin.StylesheetPath() {
		t.Errorf("both surfaces serve their stylesheet at %s", embed.StylesheetPath())
	}

	// Only the embed surface ships a script, and the admin policy has no
	// script-src to run one under.
	if embed.ScriptPath() == "" {
		t.Error("the embed surface has no script")
	}
	if admin.ScriptPath() != "" {
		t.Errorf("the admin surface ships a script at %s, and its CSP forbids running one", admin.ScriptPath())
	}
}
