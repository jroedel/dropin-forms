package mail_test

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/foundation/mail"
)

// session is what a fake relay saw.
type session struct {
	from string
	rcpt []string
	data string
}

// fakeRelay speaks just enough SMTP to accept one message, and records what it
// was told.
//
// Worth the sixty lines. The alternative is testing the message builder and
// trusting the conversation, and the conversation is where the mistakes are:
// a missing blank line between headers and body, a body that ends the DATA
// command early, an envelope sender that never got sent. None of those are
// visible without a server on the other end.
type fakeRelay struct {
	t *testing.T

	ln net.Listener

	mu       sync.Mutex
	sessions []session

	// rejectData makes the relay refuse at the moment the message is
	// finished, which is the only failure a caller can mistake for success.
	rejectData bool

	// offerStartTLS advertises an extension this fake cannot actually do, to
	// check that the client's refusal to send credentials in clear is about
	// the advertisement rather than about the address.
	offerStartTLS bool
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	r := &fakeRelay{t: t, ln: ln}

	t.Cleanup(func() { ln.Close() })

	go r.serve()

	return r
}

func (r *fakeRelay) hostPort() (string, int) {
	addr := r.ln.Addr().(*net.TCPAddr)

	return "127.0.0.1", addr.Port
}

func (r *fakeRelay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}

		go r.handle(conn)
	}
}

func (r *fakeRelay) handle(conn net.Conn) {
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	br := bufio.NewReader(conn)
	say := func(s string) { conn.Write([]byte(s + "\r\n")) }

	say("220 fake ESMTP ready")

	var s session

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}

		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			say("250-fake greets you")
			if r.offerStartTLS {
				say("250-STARTTLS")
			}
			say("250 AUTH PLAIN LOGIN")

		case strings.HasPrefix(upper, "HELO"):
			say("250 fake greets you")

		case strings.HasPrefix(upper, "AUTH"):
			say("235 authenticated")

		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.from = strings.Trim(cmd[len("MAIL FROM:"):], "<> ")
			say("250 sender ok")

		case strings.HasPrefix(upper, "RCPT TO:"):
			s.rcpt = append(s.rcpt, strings.Trim(cmd[len("RCPT TO:"):], "<> "))
			say("250 recipient ok")

		case upper == "DATA":
			say("354 go ahead")

			var body strings.Builder

			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}

				body.WriteString(l)
			}

			s.data = body.String()

			if r.rejectData {
				say("554 no thank you")

				continue
			}

			say("250 accepted")

			r.mu.Lock()
			r.sessions = append(r.sessions, s)
			r.mu.Unlock()

		case upper == "QUIT":
			say("221 bye")

			return

		case strings.HasPrefix(upper, "STARTTLS"):
			// Advertised but not implemented, on purpose: a client that takes
			// the offer gets an error rather than a clear-text session.
			say("454 not really")

		default:
			say("250 ok")
		}
	}
}

func (r *fakeRelay) last() (session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.sessions) == 0 {
		return session{}, false
	}

	return r.sessions[len(r.sessions)-1], true
}

func senderFor(t *testing.T, r *fakeRelay, adjust func(*mail.Config)) *mail.SMTP {
	t.Helper()

	host, port := r.hostPort()

	cfg := mail.Config{
		Host:     host,
		Port:     port,
		User:     "forms@schoenstatt.link",
		Password: "not a real password",
		From:     "forms@schoenstatt.link",
		FromName: "Schoenstatt Austin",
	}

	if adjust != nil {
		adjust(&cfg)
	}

	s, err := mail.NewSMTP(cfg)
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}

	return s
}

func TestSendDeliversAMessage(t *testing.T) {
	relay := newFakeRelay(t)
	s := senderFor(t, relay, nil)

	err := s.Send(t.Context(), mail.Message{
		To:      "frjeff@schoenstatt.us",
		Subject: "Sign in to Schoenstatt Austin forms",
		Text:    "Open this link to sign in.\n\nhttps://forms.schoenstatt.link/signin?t=abc\n",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, ok := relay.last()
	if !ok {
		t.Fatal("the relay never received a message")
	}

	// The envelope, which is what actually routes the mail and is separate
	// from the headers.
	if got.from != "forms@schoenstatt.link" {
		t.Errorf("envelope sender = %q", got.from)
	}
	if len(got.rcpt) != 1 || got.rcpt[0] != "frjeff@schoenstatt.us" {
		t.Errorf("envelope recipients = %v, want the one address", got.rcpt)
	}

	headers, body, found := strings.Cut(got.data, "\r\n\r\n")
	if !found {
		t.Fatalf("there is no blank line between the headers and the body:\n%q", got.data)
	}

	for _, want := range []string{
		"From: Schoenstatt Austin <forms@schoenstatt.link>",
		"To: frjeff@schoenstatt.us",
		"Subject: Sign in to Schoenstatt Austin forms",
		`Content-Type: text/plain; charset="utf-8"`,
		"MIME-Version: 1.0",

		// Not a marketing message. This is what keeps a sign-in link out of a
		// promotions tab and stops an out-of-office reply bouncing back at
		// the mailbox this service sends from.
		"Auto-Submitted: auto-generated",
		"X-Auto-Response-Suppress: All",
	} {
		if !strings.Contains(headers, want) {
			t.Errorf("the headers are missing %q:\n%s", want, headers)
		}
	}

	if !strings.Contains(headers, "Message-ID: <") || !strings.Contains(headers, "@schoenstatt.link>") {
		t.Errorf("there is no Message-ID from the sending domain:\n%s", headers)
	}
	if !strings.Contains(headers, "Date: ") {
		t.Errorf("there is no Date header:\n%s", headers)
	}

	if !strings.Contains(body, "https://forms.schoenstatt.link/signin?t=abc") {
		t.Errorf("the link is not in the body:\n%q", body)
	}

	// Every line CRLF-terminated, per RFC 5322. A bare newline is the kind of
	// thing that works against one relay and is rejected by the next.
	for line := range strings.SplitSeq(strings.TrimSuffix(got.data, "\r\n"), "\r\n") {
		if strings.Contains(line, "\n") {
			t.Errorf("a line has a bare newline in it: %q", line)
		}
	}
}

func TestSendAMultipartMessage(t *testing.T) {
	relay := newFakeRelay(t)
	s := senderFor(t, relay, nil)

	err := s.Send(t.Context(), mail.Message{
		To:      "frjeff@schoenstatt.us",
		Subject: "Your lunch tickets",
		Text:    "Thank you. Two tickets, $24.00.",
		HTML:    "<p>Thank you. Two tickets, <strong>$24.00</strong>.</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, _ := relay.last()

	if !strings.Contains(got.data, "multipart/alternative; boundary=") {
		t.Errorf("the message is not multipart:\n%s", got.data)
	}

	plainAt := strings.Index(got.data, `text/plain`)
	htmlAt := strings.Index(got.data, `text/html`)

	switch {
	case plainAt < 0 || htmlAt < 0:
		t.Fatalf("one of the two parts is missing:\n%s", got.data)

	// Plain first. A client that understands both shows the *last* part it can
	// render, so this order is what makes the HTML the one that appears --
	// reversing it silently makes every message plain.
	case plainAt > htmlAt:
		t.Error("the HTML part comes first, which makes clients show the plain text instead")
	}

	if !strings.Contains(got.data, "<strong>$24.00</strong>") {
		t.Error("the HTML body is missing")
	}
	if !strings.Contains(got.data, "Thank you. Two tickets, $24.00.") {
		t.Error("the plain text body is missing")
	}
}

// A newline in a header ends it and starts another, which is how a To field
// becomes an extra Bcc. Refused before anything is dialled.
func TestSendRefusesHeaderInjection(t *testing.T) {
	relay := newFakeRelay(t)
	s := senderFor(t, relay, nil)

	tests := []struct {
		name string
		m    mail.Message
	}{
		{
			name: "a newline in the recipient",
			m: mail.Message{
				To:      "frjeff@schoenstatt.us\nBcc: everybody@example.org",
				Subject: "Hello", Text: "Hello",
			},
		},
		{
			name: "a carriage return in the recipient",
			m: mail.Message{
				To:      "frjeff@schoenstatt.us\rBcc: everybody@example.org",
				Subject: "Hello", Text: "Hello",
			},
		},
		{
			name: "a newline in the subject",
			m: mail.Message{
				To:      "frjeff@schoenstatt.us",
				Subject: "Hello\nBcc: everybody@example.org", Text: "Hello",
			},
		},
		{name: "no recipient", m: mail.Message{Subject: "Hello", Text: "Hello"}},
		{name: "no subject", m: mail.Message{To: "a@b.co", Text: "Hello"}},

		// A message with only HTML is refused, because the plain text version
		// is the one that survives every client and every spam filter, and for
		// a sign-in link it is the one that has to be right.
		{name: "no plain text", m: mail.Message{To: "a@b.co", Subject: "Hello", HTML: "<p>Hello</p>"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.Send(t.Context(), tt.m)

			if !errors.Is(err, mail.ErrUnsendable) {
				t.Fatalf("Send returned %v, want ErrUnsendable", err)
			}

			// And nothing reached the relay, which is the point of checking
			// before dialling.
			if _, ok := relay.last(); ok {
				t.Error("a refused message was still delivered")
			}
		})
	}
}

// A body may contain anything, including the things a header may not. It is
// after the blank line and cannot escape back into the headers, and a line of
// "." is handled by the writer net/smtp returns.
func TestSendAllowsAwkwardBodies(t *testing.T) {
	relay := newFakeRelay(t)
	s := senderFor(t, relay, nil)

	body := "Here is a line with a single dot on it:\n.\nAnd text after it.\n" +
		"And something that looks like a header:\nBcc: nobody@example.org\n"

	if err := s.Send(t.Context(), mail.Message{
		To:      "frjeff@schoenstatt.us",
		Subject: "Awkward",
		Text:    body,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, ok := relay.last()
	if !ok {
		t.Fatal("the relay never received the message")
	}

	// The lone dot did not end the message early: the text after it arrived.
	if !strings.Contains(got.data, "And text after it.") {
		t.Errorf("the message was cut off at the lone dot:\n%q", got.data)
	}

	// And the header-shaped line stayed in the body, after the blank line.
	_, bodyPart, _ := strings.Cut(got.data, "\r\n\r\n")
	if !strings.Contains(bodyPart, "Bcc: nobody@example.org") {
		t.Error("the header-shaped line did not arrive in the body")
	}
	if len(got.rcpt) != 1 {
		t.Errorf("the body added a recipient: %v", got.rcpt)
	}
}

// The relay's verdict arrives when the message is closed, not when it is
// written. A deferred Close would discard a rejection and report success for
// a message nobody received.
func TestSendReportsARejectionAtTheEnd(t *testing.T) {
	relay := newFakeRelay(t)
	relay.rejectData = true

	s := senderFor(t, relay, nil)

	err := s.Send(t.Context(), mail.Message{
		To: "frjeff@schoenstatt.us", Subject: "Hello", Text: "Hello",
	})
	if err == nil {
		t.Fatal("Send reported success for a message the relay rejected")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewSMTPChecksItsConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  mail.Config
	}{
		{name: "no host", cfg: mail.Config{Port: 587, From: "a@b.co"}},
		{name: "no port", cfg: mail.Config{Host: "smtp.example.org", From: "a@b.co"}},
		{name: "a port that is not one", cfg: mail.Config{Host: "smtp.example.org", Port: 70000, From: "a@b.co"}},
		{name: "no sender", cfg: mail.Config{Host: "smtp.example.org", Port: 587}},
		{
			name: "a line break in the sender",
			cfg:  mail.Config{Host: "smtp.example.org", Port: 587, From: "a@b.co\nBcc: c@d.co"},
		},
		{
			name: "a line break in the display name",
			cfg:  mail.Config{Host: "smtp.example.org", Port: 587, From: "a@b.co", FromName: "A\nB"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Checked at construction rather than at the first send, so a
			// typo in the relay address is a startup failure instead of a
			// sign-in that silently never arrives.
			if _, err := mail.NewSMTP(tt.cfg); err == nil {
				t.Error("NewSMTP accepted it")
			}
		})
	}
}

// A relay that offers STARTTLS and then fails it must not fall back to
// clear text, because the next thing on the wire would be a password.
func TestSendWillNotFallBackToClearTextWhenTLSWasOffered(t *testing.T) {
	relay := newFakeRelay(t)
	relay.offerStartTLS = true

	s := senderFor(t, relay, nil)

	err := s.Send(t.Context(), mail.Message{
		To: "frjeff@schoenstatt.us", Subject: "Hello", Text: "Hello",
	})
	if err == nil {
		t.Fatal("Send succeeded although the TLS upgrade failed")
	}
	if !strings.Contains(err.Error(), "secured") {
		t.Errorf("unexpected error: %v", err)
	}

	if _, ok := relay.last(); ok {
		t.Error("a message went out over a connection that failed to upgrade")
	}
}

func TestSendReportsAnUnreachableRelay(t *testing.T) {
	// Port 1 on loopback: nothing listens there, and it fails immediately
	// rather than timing out, so this test is fast and not flaky.
	s, err := mail.NewSMTP(mail.Config{
		Host: "127.0.0.1", Port: 1, From: "forms@schoenstatt.link",
	})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}

	err = s.Send(t.Context(), mail.Message{
		To: "frjeff@schoenstatt.us", Subject: "Hello", Text: "Hello",
	})
	if err == nil {
		t.Fatal("Send succeeded with nothing listening")
	}
	if !strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRecorderKeepsMessages(t *testing.T) {
	var r mail.Recorder

	if _, ok := r.Last(); ok {
		t.Error("a fresh recorder has a last message")
	}

	for _, subject := range []string{"one", "two"} {
		if err := r.Send(t.Context(), mail.Message{
			To: "a@b.co", Subject: subject, Text: "hello",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if len(r.Sent) != 2 {
		t.Errorf("recorded %d messages, want 2", len(r.Sent))
	}

	last, ok := r.Last()
	if !ok || last.Subject != "two" {
		t.Errorf("Last() = %+v, %v; want the second message", last, ok)
	}

	// It satisfies the interface the app layer depends on, which is the whole
	// reason it is here and not in a test file.
	var _ mail.Sender = &r
}
