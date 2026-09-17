// Package mail sends messages through an authenticated SMTP relay.
//
// It knows nothing about what it is sending or why. Everything here is
// envelopes, headers and timeouts.
//
// # Why this does not call smtp.SendMail
//
// smtp.SendMail takes an address and nothing else: no context, no dialer, no
// deadline. It calls net.Dial, which waits for the operating system's
// connect timeout -- minutes -- and then reads from the connection with no
// deadline at all. A relay that accepts a connection and then says nothing
// would hold the request that triggered it open indefinitely, and on the
// sign-in path that is a page that never loads. So this dials with a context
// and sets a deadline covering the whole conversation, and drives the client
// itself.
//
// # What it refuses
//
// Any address, name or subject containing a carriage return or newline. A
// header is terminated by CRLF, so a newline inside one ends it and starts
// another -- which is how a "To" field becomes an extra "Bcc". The message
// body is exempt, because the body is after the blank line and cannot escape
// back into the headers; the dot-stuffing that keeps a line of "." in the body
// from ending the message early is handled by the writer net/smtp returns.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Timeouts for one message. Generous, because a relay under load is slow
// rather than broken, and bounded, because a request is waiting.
const (
	dialTimeout    = 10 * time.Second
	sessionTimeout = 30 * time.Second
)

// ErrUnsendable is a message this package refuses to send, as opposed to one
// the relay rejected. It means a bug or an injection attempt in the caller,
// never a network problem.
var ErrUnsendable = errors.New("that message cannot be sent")

// Config is where to send through.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string

	// From is the envelope sender and the From header. It must be an address
	// the relay is willing to send as, and one whose domain passes SPF and
	// DKIM -- mail sent from a domain that does not authorise the relay lands
	// in spam, and for a sign-in link that means nobody can log in.
	From string

	// FromName is the display name, optional.
	FromName string
}

// Message is one message to one recipient.
//
// One recipient, deliberately. Every message this service sends is personal --
// a sign-in link, a receipt, a notification -- and a list of recipients on one
// envelope is how everybody learns who else is on it.
type Message struct {
	To      string
	Subject string

	// Text is required. HTML is optional, and when present the message goes
	// out as multipart/alternative.
	//
	// Text is not a fallback nobody reads. A sign-in link in a plain-text
	// message is the version that survives every client, every screen reader
	// and every spam filter, so it is the one that has to be right.
	Text string
	HTML string
}

// Sender is what the app layer depends on, so that it can be given something
// that records instead of sends.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP sends through a relay.
type SMTP struct {
	cfg  Config
	addr string
}

// NewSMTP checks the configuration and returns a sender.
//
// Checked here rather than at the first send, so that a typo in the relay
// address is a startup failure instead of a sign-in that silently never
// arrives.
func NewSMTP(cfg Config) (*SMTP, error) {
	switch {
	case cfg.Host == "":
		return nil, errors.New("the mail relay has no host")
	case cfg.Port <= 0 || cfg.Port > 65535:
		return nil, fmt.Errorf("the mail relay port %d is not a port", cfg.Port)
	case cfg.From == "":
		return nil, errors.New("there is no address to send mail from")
	}

	for name, value := range map[string]string{
		"the address to send from": cfg.From,
		"the name to send as":      cfg.FromName,
		"the relay host":           cfg.Host,
		"the relay username":       cfg.User,
	} {
		if hasLineBreak(value) {
			return nil, fmt.Errorf("%w: %s contains a line break", ErrUnsendable, name)
		}
	}

	return &SMTP{
		cfg:  cfg,
		addr: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)),
	}, nil
}

// Send delivers one message.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	body, err := s.build(m)
	if err != nil {
		return err
	}

	d := net.Dialer{Timeout: dialTimeout}

	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("the mail relay at %s could not be reached: %w", s.addr, err)
	}
	defer conn.Close()

	// One deadline for the whole conversation. net/smtp has no other way to
	// be interrupted: it does not take a context, so a relay that stops
	// responding mid-DATA is only escapable through the connection itself.
	if err := conn.SetDeadline(time.Now().Add(sessionTimeout)); err != nil {
		return fmt.Errorf("the connection to the mail relay could not be bounded: %w", err)
	}

	// Cancelling the context closes the connection, which makes the blocked
	// read fail. Without this, a cancelled request still waits out the
	// deadline above.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("the mail relay did not greet us: %w", err)
	}
	defer c.Close()

	if err := s.secure(c); err != nil {
		return err
	}

	if s.cfg.User != "" {
		// PlainAuth refuses to hand over a password over an unencrypted
		// connection unless the host is loopback, which is a check worth
		// having rather than working around.
		auth := smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("the mail relay refused our credentials: %w", err)
		}
	}

	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("the mail relay refused the sender %s: %w", s.cfg.From, err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("the mail relay refused the recipient: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the mail relay would not take the message: %w", err)
	}

	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("the message could not be written: %w", err)
	}

	// Closing is what sends it, and its error is the relay's verdict -- so it
	// is checked rather than deferred. A deferred Close here would discard a
	// rejection and report success for a message nobody received.
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail relay rejected the message: %w", err)
	}

	if err := c.Quit(); err != nil {
		// The message is already accepted at this point, so a bad goodbye is
		// not a failure to deliver.
		return nil
	}

	return nil
}

// secure upgrades the connection, and requires it off the loopback.
func (s *SMTP) secure(c *smtp.Client) error {
	ok, _ := c.Extension("STARTTLS")

	if !ok {
		// A relay reachable only over loopback is either a local test double
		// or a submission agent on the same host, and in neither case does
		// TLS add anything. Anywhere else, refusing is the only safe answer:
		// continuing would put a relay password on the wire.
		if isLoopback(s.cfg.Host) {
			return nil
		}

		return fmt.Errorf("the mail relay at %s does not offer TLS, and this will not send credentials without it", s.addr)
	}

	if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
		return fmt.Errorf("the connection to the mail relay could not be secured: %w", err)
	}

	return nil
}

// build renders the message. Exported behaviour is tested through this, since
// the bytes on the wire are the thing that either arrives or does not.
func (s *SMTP) build(m Message) ([]byte, error) {
	switch {
	case m.To == "":
		return nil, fmt.Errorf("%w: it has no recipient", ErrUnsendable)
	case m.Subject == "":
		return nil, fmt.Errorf("%w: it has no subject", ErrUnsendable)
	case m.Text == "":
		return nil, fmt.Errorf("%w: it has no plain text version", ErrUnsendable)
	case hasLineBreak(m.To):
		return nil, fmt.Errorf("%w: the recipient contains a line break", ErrUnsendable)
	case hasLineBreak(m.Subject):
		return nil, fmt.Errorf("%w: the subject contains a line break", ErrUnsendable)
	}

	var b strings.Builder

	from := s.cfg.From
	if s.cfg.FromName != "" {
		// RFC 2047 for the display name, so that a name with an accent in it
		// is not mangled. mime.QEncoding leaves plain ASCII alone, so the
		// common case stays readable in a raw message.
		from = mime.QEncoding.Encode("utf-8", s.cfg.FromName) + " <" + s.cfg.From + ">"
	}

	headers := [][2]string{
		{"From", from},
		{"To", m.To},
		{"Subject", mime.QEncoding.Encode("utf-8", m.Subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", s.messageID()},
		{"MIME-Version", "1.0"},

		// Not a marketing message, and saying so is what keeps a sign-in link
		// out of a "promotions" tab and stops an out-of-office reply bouncing
		// back at the mailbox this service sends from.
		{"Auto-Submitted", "auto-generated"},
		{"X-Auto-Response-Suppress", "All"},
	}

	if m.HTML == "" {
		headers = append(headers,
			[2]string{"Content-Type", `text/plain; charset="utf-8"`},
			[2]string{"Content-Transfer-Encoding", "8bit"},
		)

		writeHeaders(&b, headers)
		b.WriteString("\r\n")
		b.WriteString(normaliseBody(m.Text))

		return []byte(b.String()), nil
	}

	var alt strings.Builder
	mp := multipart.NewWriter(&alt)

	headers = append(headers, [2]string{
		"Content-Type", `multipart/alternative; boundary="` + mp.Boundary() + `"`,
	})

	// Plain text first. A client that understands both is specified to show
	// the last part it can render, so this order is what makes the HTML the
	// one that appears -- and reversing it silently makes every message
	// plain.
	for _, part := range []struct{ kind, content string }{
		{`text/plain; charset="utf-8"`, m.Text},
		{`text/html; charset="utf-8"`, m.HTML},
	} {
		w, err := mp.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.kind},
			"Content-Transfer-Encoding": {"8bit"},
		})
		if err != nil {
			return nil, fmt.Errorf("the message could not be assembled: %w", err)
		}

		if _, err := w.Write([]byte(normaliseBody(part.content))); err != nil {
			return nil, fmt.Errorf("the message could not be assembled: %w", err)
		}
	}

	if err := mp.Close(); err != nil {
		return nil, fmt.Errorf("the message could not be assembled: %w", err)
	}

	writeHeaders(&b, headers)
	b.WriteString("\r\n")
	b.WriteString(alt.String())

	return []byte(b.String()), nil
}

func (s *SMTP) messageID() string {
	_, domain, found := strings.Cut(s.cfg.From, "@")
	if !found {
		domain = s.cfg.Host
	}

	return "<" + strings.ToLower(rand.Text()) + "@" + domain + ">"
}

func writeHeaders(b *strings.Builder, headers [][2]string) {
	for _, h := range headers {
		b.WriteString(h[0])
		b.WriteString(": ")
		b.WriteString(h[1])
		b.WriteString("\r\n")
	}
}

// normaliseBody gives every line a CRLF ending.
//
// Required by RFC 5322, and the writer net/smtp hands back translates a bare
// newline anyway -- but doing it here means the bytes this package builds are
// a valid message on their own, which is what makes them testable without a
// relay.
func normaliseBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")

	return strings.ReplaceAll(s, "\n", "\r\n")
}

func hasLineBreak(s string) bool { return strings.ContainsAny(s, "\r\n") }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// Recorder is a Sender that keeps messages instead of sending them, for tests
// and for a development run with no relay configured.
//
// Deliberately in this package rather than in a test file: a service whose
// only way in is an emailed link needs a way to run without a relay, and
// hiding that in _test.go means the development path and the tested path are
// different code.
type Recorder struct {
	Sent []Message
}

// Send records the message. It still builds nothing and checks nothing, so a
// caller cannot rely on it to catch an unsendable message -- that is what
// [SMTP] is for.
func (r *Recorder) Send(_ context.Context, m Message) error {
	r.Sent = append(r.Sent, m)

	return nil
}

// Last returns the most recent message, which is what a test almost always
// wants.
func (r *Recorder) Last() (Message, bool) {
	if len(r.Sent) == 0 {
		return Message{}, false
	}

	return r.Sent[len(r.Sent)-1], true
}
