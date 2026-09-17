package types

import (
	"errors"
	"fmt"
	"strings"
)

// Email is an address whose shape has been checked, normalised for use as an
// identity.
//
// "Checked" is the honest word rather than "valid". Whether an address can
// receive mail is not knowable from its text, and the only test that settles
// it is sending something -- which this service does anyway, since signing in
// means clicking a link that arrived. So the job here is to catch a typo and
// to refuse anything that could do damage further along, not to adjudicate
// RFC 5322.
type Email struct {
	addr string
}

// ErrNotAnEmail is what every parse failure wraps.
var ErrNotAnEmail = errors.New("not an email address")

// The SMTP limits. Above them no relay will take the message anyway, so
// storing such an address only defers the failure to a worse moment.
const (
	emailMaxLen = 254
	localMaxLen = 64
)

// ParseEmail checks the shape of an address and normalises it to lower case.
//
// Not net/mail.ParseAddress, which accepts `Jane Doe <jane@example.com>`, a
// bare bracketed address, and a quoted local part with spaces in it. All three
// are correct for a mail header and wrong for a field labelled "Email": a
// display name means storing something that is not an address, and then
// mailing a sign-in link to a string nobody can read.
//
// # Why the whole address is lower-cased
//
// RFC 5321 makes the local part case-sensitive, so in principle Jeff@ and
// jeff@ may be different people. In practice no provider works that way, and
// the cost of honouring the standard is that one person signing in as Jeff@
// and later as jeff@ gets two accounts, each with its own per-form roles --
// discovered as "my permissions disappeared". Folding is the lesser wrong, and
// it is what makes an address usable as a unique key.
//
// The consequence to know about: an address that really does depend on the
// case of its local part cannot hold an account here.
func ParseEmail(s string) (Email, error) {
	if s == "" {
		return Email{}, fmt.Errorf("%w: it is blank", ErrNotAnEmail)
	}

	// Trimmed, unlike ParseMoney. An address pasted out of a mail client or a
	// spreadsheet arrives with a space on it constantly, and there is nothing
	// an attacker gains from one -- whereas refusing it means telling somebody
	// their own address is wrong.
	s = strings.TrimSpace(s)

	if len(s) > emailMaxLen {
		return Email{}, fmt.Errorf("%w: it is longer than %d characters", ErrNotAnEmail, emailMaxLen)
	}

	local, domain, found := strings.Cut(s, "@")
	if !found {
		return Email{}, fmt.Errorf("%w: %q has no @ in it", ErrNotAnEmail, s)
	}

	switch {
	case local == "":
		return Email{}, fmt.Errorf("%w: %q has nothing before the @", ErrNotAnEmail, s)
	case len(local) > localMaxLen:
		return Email{}, fmt.Errorf("%w: the part before the @ is longer than %d characters", ErrNotAnEmail, localMaxLen)
	case domain == "":
		return Email{}, fmt.Errorf("%w: %q has nothing after the @", ErrNotAnEmail, s)
	case strings.Contains(domain, "@"):
		return Email{}, fmt.Errorf("%w: %q has more than one @", ErrNotAnEmail, s)
	}

	// An apostrophe is deliberately absent from this list. It is legal in a
	// local part and it is in real surnames -- o'neill@example.org is
	// somebody's actual address, and refusing it turns away a person rather
	// than an attack. Everything here either breaks a mail header, separates
	// one address from the next, or is a display name trying to get in.
	if i := strings.IndexAny(s, " \t\r\n\"(),:;<>[]\\"); i >= 0 {
		return Email{}, fmt.Errorf("%w: %q contains %q, so it is either two addresses or has a name attached", ErrNotAnEmail, s, s[i:i+1])
	}

	if err := checkDomain(domain); err != nil {
		return Email{}, err
	}

	return Email{addr: strings.ToLower(s)}, nil
}

func checkDomain(domain string) error {
	switch {
	case len(domain) > 255:
		return fmt.Errorf("%w: the part after the @ is too long", ErrNotAnEmail)
	case strings.HasPrefix(domain, "."), strings.HasSuffix(domain, "."):
		return fmt.Errorf("%w: %q begins or ends with a dot", ErrNotAnEmail, domain)
	case strings.Contains(domain, ".."):
		return fmt.Errorf("%w: %q has two dots in a row", ErrNotAnEmail, domain)
	case strings.HasPrefix(domain, "-"), strings.HasSuffix(domain, "-"):
		return fmt.Errorf("%w: %q begins or ends with a hyphen", ErrNotAnEmail, domain)
	}

	// A domain with no dot is syntactically legal -- `root@localhost` is a
	// real address on a real machine -- and is never a real recipient for
	// this service. It is almost always a dropped ".com".
	label, rest, hasDot := strings.Cut(domain, ".")
	if !hasDot || label == "" || rest == "" {
		return fmt.Errorf("%w: %q is not a domain name; it needs a dot, like example.org", ErrNotAnEmail, domain)
	}

	for part := range strings.SplitSeq(domain, ".") {
		if part == "" {
			return fmt.Errorf("%w: %q has an empty part", ErrNotAnEmail, domain)
		}

		for i := range len(part) {
			c := part[i]

			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				// Explicitly ASCII. An internationalised domain has to arrive
				// already in its punycode form: accepting the Unicode
				// spelling would mean two addresses that look identical in a
				// list are different accounts, which is the homoglyph problem
				// with a login attached.
				return fmt.Errorf("%w: %q is not a domain name this service can use; if it is not written in plain letters, use its punycode form", ErrNotAnEmail, domain)
			}
		}
	}

	return nil
}

// String is the normalised address, which is what to store and what to send to.
func (e Email) String() string { return e.addr }

// Domain is the part after the @, lower-cased. Useful for a per-domain rate
// limit, which is a cheaper signal than a per-address one when somebody is
// working through a list.
func (e Email) Domain() string {
	_, domain, _ := strings.Cut(e.addr, "@")

	return domain
}

// Zero reports whether this is the unset Email.
func (e Email) Zero() bool { return e.addr == "" }
