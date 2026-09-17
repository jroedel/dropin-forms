// Package page holds what the two surfaces put in a <head> and in their
// response headers.
//
// It is one layer above foundation/ because these values know domain words --
// a form's allowed embedding origins, most obviously -- and nothing in
// foundation/ may. foundation/web.SecureHeaders takes the policy as a value
// for exactly this reason; the shortcut of importing formbus into foundation/
// is forbidden, and this comment is here so that nobody has to rediscover why.
package page

import (
	"net/http"
	"strings"

	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// hsts is sent on both surfaces.
//
// Apache already adds one at the proxy, and schoenstatt.link sends it with
// includeSubDomains and preload, so in production this is belt and braces. It
// is here anyway because a page that takes a payment and is framed on other
// people's websites should not depend on a .htaccess staying correct, and
// because the parent project's header set had no HSTS at all.
const hsts = "max-age=31536000; includeSubDomains"

// FrameAncestorsFor answers which origins may frame this request's page.
//
// It is a function because the answer is per form, and the form is identified
// by the request. Returning nothing means nothing may frame it: the policy
// below fails closed, which matters more than it looks. A CSP that simply
// omits frame-ancestors is frameable by the entire web -- default-src 'none'
// does not restrict framing -- so an empty list must become 'none' rather than
// an absent directive.
type FrameAncestorsFor func(*http.Request) []types.Origin

// EmbedPolicy is the policy for the public, embeddable form surface.
//
// This is the surface the framing decision in docs/design/drop-in-forms.md is
// about. Two things it does *not* have are worth naming, because both look
// like omissions:
//
//   - No X-Frame-Options. It cannot express a list, ALLOW-FROM is dead, and
//     any value at all would kill the embed in browsers that honour it.
//   - No Stripe origin anywhere. Payment goes through Stripe-hosted Checkout,
//     so no third-party script loads on our own page. When the in-page Payment
//     Element eventually replaces Checkout, this is the function that grows
//     five origins, and that change deserves the Report-Only rollout described
//     in section 7.2 rather than a quiet edit.
//
// Cache-Control is no-store, which reverses a line in the web skill. The skill
// is right in general and wrong for this page: it carries a single-use
// submission grant, and a shared cache handing the same nonce to fifty
// visitors breaks the one property the grant has.
func EmbedPolicy(frameAncestors FrameAncestorsFor) web.PolicyFor {
	return func(r *http.Request) web.Policy {
		return web.Policy{
			ContentSecurityPolicy: strings.Join([]string{
				"default-src 'none'",
				"script-src 'self'",
				"style-src 'self'",
				"img-src 'self' data:",
				"form-action 'self'",
				"base-uri 'none'",
				"frame-ancestors " + frameAncestorList(frameAncestors, r),
			}, "; "),

			// same-origin rather than no-referrer, which is what the parent
			// project used. no-referrer makes a browser send `Origin: null` on
			// a native form submission, which broke the Origin fallback in the
			// same-origin check. Sending our own origin to our own pages and
			// nothing to anybody else's is the behaviour we actually wanted.
			ReferrerPolicy:  "same-origin",
			CacheControl:    "no-store",
			StrictTransport: hsts,
		}
	}
}

// AdminPolicy is the policy for the management surface.
//
// It keeps the strict policy the web skill ships, because the reasoning behind
// it is untouched here: a server-rendered admin app genuinely has no scripts
// and no remote assets, and a page that cannot make a network request cannot
// exfiltrate anything, whatever ends up injected into a field somebody typed.
// There is no script-src at all, and adding one should feel like a decision.
//
// frame-ancestors is 'none'. The management app is never embedded, and the
// whole reason it lives on a second hostname is to keep it away from the
// surface that is.
func AdminPolicy() web.PolicyFor {
	return func(*http.Request) web.Policy {
		return web.Policy{
			ContentSecurityPolicy: strings.Join([]string{
				"default-src 'none'",
				"style-src 'self'",
				"img-src 'self' data:",
				"form-action 'self'",
				"base-uri 'none'",
				"frame-ancestors 'none'",
			}, "; "),

			ReferrerPolicy: "same-origin",

			// A page showing somebody's submitted answers, including an email
			// address and what they paid, has no business in any cache.
			CacheControl:    "no-store",
			FrameOptions:    "DENY",
			StrictTransport: hsts,
		}
	}
}

// frameAncestorList renders the directive's value, failing closed.
func frameAncestorList(frameAncestors FrameAncestorsFor, r *http.Request) string {
	if frameAncestors == nil {
		return "'none'"
	}

	origins := frameAncestors(r)
	if len(origins) == 0 {
		return "'none'"
	}

	parts := make([]string, 0, len(origins))
	for _, o := range origins {
		if !o.Zero() {
			parts = append(parts, o.String())
		}
	}

	if len(parts) == 0 {
		return "'none'"
	}

	return strings.Join(parts, " ")
}
