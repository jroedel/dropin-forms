package types

import (
	"fmt"
	"net/url"
	"strings"
)

// Origin is a web origin that has been checked well enough to be written into
// a Content-Security-Policy header.
//
// It is a type rather than a string because the value arrives from a form
// owner typing the address of their own website, and it ends up in the CSP of
// a page that leads to a payment. The attack is not the one people expect:
// Go's response writer replaces CR and LF in a header value with *spaces*, so
// classic header injection is out -- but a space is a perfectly good CSP
// separator, so an entry of
//
//	https://example.test; script-src 'unsafe-inline'
//
// would inject a directive into our own page. The defence is not to escape the
// input; it is to parse it, keep only the scheme and host, and re-serialise
// from those parts, so that whatever was typed cannot survive into the header.
type Origin struct {
	host string
}

// disallowed are the characters that have no business in a host and every
// business in a CSP. The check runs on the raw string before parsing, because
// url.Parse is lenient in ways that are fine for a URL and not fine here.
const disallowed = " \t\r\n\"'`;,()<>{}[]|\\^"

// ParseOrigin accepts `https://host` or `https://*.host` and nothing else.
//
// https is required rather than merely preferred: everything here is HTTPS-only
// behind HSTS with includeSubDomains, so a plaintext origin is either a mistake
// or an attempt, and neither should be stored. A scheme-less host-source would
// also match loosely in a CSP, which is one more thing that would have to stay
// true.
func ParseOrigin(s string) (Origin, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Origin{}, fmt.Errorf("an allowed website address cannot be blank")
	}

	if strings.ContainsAny(s, disallowed) {
		return Origin{}, fmt.Errorf("%q is not a website address: write it as https://example.org, with no path and no punctuation after the host", s)
	}

	u, err := url.Parse(s)
	if err != nil {
		return Origin{}, fmt.Errorf("%q is not a website address: write it as https://example.org", s)
	}

	if u.Scheme != "https" {
		return Origin{}, fmt.Errorf("%q must start with https:// -- an embedded form is only served over https", s)
	}

	if u.User != nil {
		return Origin{}, fmt.Errorf("%q must not contain a username or password", s)
	}

	// A path, query or fragment is not merely ignored by frame-ancestors -- it
	// is a sign that somebody pasted a page URL rather than a site address,
	// and silently dropping it would leave them believing they had restricted
	// something they had not.
	if u.Path != "" && u.Path != "/" {
		return Origin{}, fmt.Errorf("%q must be the site address only, with no path: use https://%s", s, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Origin{}, fmt.Errorf("%q must be the site address only, with no ? or # part: use https://%s", s, u.Host)
	}

	host := u.Host
	if host == "" {
		return Origin{}, fmt.Errorf("%q is missing the website name: write it as https://example.org", s)
	}

	// One leading `*.` wildcard label is allowed, because a form owner may
	// legitimately need to cover a staging subdomain. A bare `*`, or a star
	// anywhere else, is not -- `https://*` is every site there is.
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		if rest == "" || strings.Contains(rest, "*") {
			return Origin{}, fmt.Errorf("%q may use a wildcard only as the first label, as in https://*.example.org", s)
		}
	} else if strings.Contains(host, "*") {
		return Origin{}, fmt.Errorf("%q may use a wildcard only as the first label, as in https://*.example.org", s)
	}

	return Origin{host: strings.ToLower(host)}, nil
}

// String re-serialises from the parsed host. Nothing the caller typed reaches
// the header except a hostname that survived ParseOrigin.
func (o Origin) String() string {
	if o.host == "" {
		return ""
	}

	return "https://" + o.host
}

// Zero reports whether this is the empty Origin, which is what a caller gets
// back alongside an error.
func (o Origin) Zero() bool { return o.host == "" }
