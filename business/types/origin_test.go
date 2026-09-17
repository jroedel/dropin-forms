package types_test

import (
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types"
)

func TestParseOriginAccepts(t *testing.T) {
	tests := map[string]string{
		"plain host":        "https://example.org",
		"subdomain":         "https://www.example.org",
		"one wildcard":      "https://*.example.org",
		"trailing slash":    "https://example.org/",
		"surrounding space": "  https://example.org  ",
		"uppercase host":    "https://EXAMPLE.org",
		"port":              "https://example.org:8443",
		"the real site":     "https://schoenstatt-austin.us",
	}

	want := map[string]string{
		"plain host":        "https://example.org",
		"subdomain":         "https://www.example.org",
		"one wildcard":      "https://*.example.org",
		"trailing slash":    "https://example.org",
		"surrounding space": "https://example.org",
		"uppercase host":    "https://example.org",
		"port":              "https://example.org:8443",
		"the real site":     "https://schoenstatt-austin.us",
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := types.ParseOrigin(in)
			if err != nil {
				t.Fatalf("ParseOrigin(%q) = %v, want it accepted", in, err)
			}

			if got.String() != want[name] {
				t.Errorf("ParseOrigin(%q).String() = %q, want %q", in, got.String(), want[name])
			}
		})
	}
}

// TestParseOriginRejects is the security test, not a validation test.
//
// Every entry below, if it reached a Content-Security-Policy header, would
// either inject a directive into a page that leads to a payment or widen the
// set of sites allowed to frame one. Go replaces CR and LF in a header value
// with spaces, and a space separates CSP source expressions -- so the newline
// cases are not stopped by the response writer, they are stopped here.
func TestParseOriginRejects(t *testing.T) {
	tests := map[string]string{
		"empty":                "",
		"whitespace only":      "   ",
		"directive injection":  "https://a.test; script-src 'unsafe-inline'",
		"newline injection":    "https://a.test\nscript-src 'unsafe-inline'",
		"carriage return":      "https://a.test\rscript-src 'unsafe-inline'",
		"second source":        "https://a.test https://evil.test",
		"comma separated":      "https://a.test,https://evil.test",
		"quoted keyword":       "'unsafe-inline'",
		"bare wildcard":        "https://*",
		"wildcard scheme":      "*",
		"interior wildcard":    "https://a.*.test",
		"trailing wildcard":    "https://a.test.*",
		"plain http":           "http://a.test",
		"no scheme":            "a.test",
		"scheme relative":      "//a.test",
		"data url":             "data:text/html,x",
		"with path":            "https://a.test/embed",
		"with query":           "https://a.test?x=1",
		"with fragment":        "https://a.test#x",
		"with userinfo":        "https://user:pw@a.test",
		"missing host":         "https://",
		"backtick":             "https://a.test`x",
		"parenthesis":          "https://a.test(x)",
		"angle bracket":        "https://a.test<x>",
		"tab separated source": "https://a.test\thttps://evil.test",
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := types.ParseOrigin(in)
			if err == nil {
				t.Fatalf("ParseOrigin(%q) was accepted as %q, want it refused", in, got.String())
			}

			if !got.Zero() {
				t.Errorf("ParseOrigin(%q) returned an error and a non-zero Origin %q; a refused value must not be usable", in, got.String())
			}
		})
	}
}

// TestOriginStringIsAlwaysOneSource guards the property the whole type exists
// for: whatever was accepted, the serialised form is a single CSP source
// expression and cannot be read as two, or as a source plus a directive.
func TestOriginStringIsAlwaysOneSource(t *testing.T) {
	inputs := []string{
		"https://example.org",
		"https://*.example.org",
		"https://example.org:8443",
		"  https://EXAMPLE.org/  ",
	}

	for _, in := range inputs {
		o, err := types.ParseOrigin(in)
		if err != nil {
			t.Fatalf("ParseOrigin(%q) = %v", in, err)
		}

		s := o.String()
		if strings.ContainsAny(s, " \t\r\n;,'\"") {
			t.Errorf("ParseOrigin(%q).String() = %q, which is not a single CSP source", in, s)
		}
		if !strings.HasPrefix(s, "https://") {
			t.Errorf("ParseOrigin(%q).String() = %q, want an https origin", in, s)
		}
	}
}
