package vendor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// SMTP sends email through a tenant's SMTP server. TLS is mandatory
// (implicit TLS, or STARTTLS that the server must offer); the host is
// re-checked against the save-time rule and every connection goes through
// the netguard dialer (the resolved address must be public).
type SMTP struct {
	// Dial opens the TCP connection (default: the guard's dialer).
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// RootCAs verifies server certificates (nil: system roots).
	RootCAs *x509.CertPool
	// Hostname is the EHLO name.
	Hostname string
	Timeout  time.Duration
	Now      func() time.Time
}

// NewSMTP returns an SMTP sender that dials through g.
func NewSMTP(g *netguard.Guard, hostname string) *SMTP {
	return &SMTP{
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return g.DialContext(ctx, "tcp", addr, 10*time.Second)
		},
		Hostname: hostname, Timeout: 30 * time.Second, Now: time.Now,
	}
}

func cfgInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func headerSafe(ss ...string) bool {
	for _, s := range ss {
		if strings.ContainsAny(s, "\r\n\x00") {
			return false
		}
	}
	return true
}

// Send implements channel.Sender.
func (s *SMTP) Send(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	if m.Email == nil {
		return channel.Result{}, channel.Permanent("render_failed", "no email content")
	}
	if m.To == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no email address")
	}
	host, port, mode := strings.TrimSuffix(strings.ToLower(cfgStr(c.Config, "host")), "."), cfgInt(c.Config, "port"), cfgStr(c.Config, "tls")
	if credential.SMTPHost(host) != nil || port <= 0 || port > 65535 || (mode != "starttls" && mode != "implicit") {
		return channel.Result{}, channel.Permanent("credential_invalid", "the SMTP credential's host, port or TLS mode is invalid")
	}
	from := cfgStr(c.Config, "from_email")
	if !headerSafe(from, m.To, m.ReplyTo, m.Email.Subject) {
		return channel.Result{}, channel.Permanent("invalid_content", "a header field contains a line break")
	}
	raw, msgID, err := s.compose(m, from, fromName(m, c))
	if err != nil {
		return channel.Result{}, channel.Permanent("render_failed", "the email could not be encoded")
	}

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	conn, err := s.Dial(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: s.RootCAs}
	if mode == "implicit" {
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return channel.Result{}, smtpError(err)
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return channel.Result{}, smtpError(err)
	}
	defer func() { _ = cl.Close() }()
	if err := cl.Hello(s.Hostname); err != nil {
		return channel.Result{}, smtpError(err)
	}
	if mode == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return channel.Result{}, channel.Permanent("tls_unavailable", "the SMTP server does not offer STARTTLS; Notifex never sends in plaintext")
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return channel.Result{}, smtpError(err)
		}
	}
	if user := cfgStr(c.Config, "username"); user != "" {
		if err := cl.Auth(smtp.PlainAuth("", user, cfgStr(c.Secret, "password"), host)); err != nil {
			return channel.Result{}, smtpError(err)
		}
	}
	if err := cl.Mail(from); err != nil {
		return channel.Result{}, smtpError(err)
	}
	if err := cl.Rcpt(m.To); err != nil {
		return channel.Result{}, smtpRcptError(err)
	}
	w, err := cl.Data()
	if err != nil {
		return channel.Result{}, smtpError(err)
	}
	if _, err := w.Write(raw); err != nil {
		return channel.Result{}, smtpError(err)
	}
	if err := w.Close(); err != nil {
		return channel.Result{}, smtpError(err)
	}
	_ = cl.Quit()
	return channel.Result{VendorMessageID: msgID}, nil
}

func (s *SMTP) compose(m channel.Message, from, name string) ([]byte, string, error) {
	msgID := m.NotificationID.String() + "@notifex"
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", (&mail.Address{Name: name, Address: from}).String())
	h("To", (&mail.Address{Address: m.To}).String())
	if m.ReplyTo != "" {
		h("Reply-To", (&mail.Address{Address: m.ReplyTo}).String())
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Email.Subject))
	h("Date", s.Now().UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+msgID+">")
	h("MIME-Version", "1.0")
	h("X-Notifex-Notification-Id", m.NotificationID.String())

	part := func(w *bytes.Buffer, body string) error {
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(body)); err != nil {
			return fmt.Errorf("quoted-printable: %w", err)
		}
		if err := qp.Close(); err != nil {
			return fmt.Errorf("quoted-printable: %w", err)
		}
		return nil
	}
	switch {
	case m.Email.Text != "" && m.Email.HTML != "":
		boundary := make([]byte, 12)
		_, _ = rand.Read(boundary)
		mw := multipart.NewWriter(&b)
		if err := mw.SetBoundary("nfx-" + hex.EncodeToString(boundary)); err != nil {
			return nil, "", fmt.Errorf("boundary: %w", err)
		}
		h("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
		b.WriteString("\r\n")
		for _, p := range []struct{ typ, body string }{{"text/plain", m.Email.Text}, {"text/html", m.Email.HTML}} {
			pw, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {p.typ + "; charset=utf-8"},
				"Content-Transfer-Encoding": {"quoted-printable"},
			})
			if err != nil {
				return nil, "", fmt.Errorf("part: %w", err)
			}
			var pb bytes.Buffer
			if err := part(&pb, p.body); err != nil {
				return nil, "", err
			}
			if _, err := pw.Write(pb.Bytes()); err != nil {
				return nil, "", fmt.Errorf("part: %w", err)
			}
		}
		if err := mw.Close(); err != nil {
			return nil, "", fmt.Errorf("multipart: %w", err)
		}
	default:
		typ, body := "text/plain", m.Email.Text
		if body == "" {
			typ, body = "text/html", m.Email.HTML
		}
		h("Content-Type", typ+"; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := part(&b, body); err != nil {
			return nil, "", err
		}
	}
	return b.Bytes(), msgID, nil
}

// smtpError classifies SMTP replies: 4xx retryable, 5xx permanent (530,
// 534, 535 are auth failures); certificate problems are permanent.
func smtpError(err error) *channel.SendError {
	var te *textproto.Error
	if errors.As(err, &te) {
		switch {
		case te.Code >= 400 && te.Code < 500:
			return channel.Retryable("smtp_temporary", fmt.Sprintf("the SMTP server deferred the message (%d)", te.Code))
		case te.Code == 530 || te.Code == 534 || te.Code == 535:
			return channel.Permanent("auth_failed", fmt.Sprintf("the SMTP server refused the login (%d)", te.Code))
		default:
			return channel.Permanent("smtp_rejected", fmt.Sprintf("the SMTP server rejected the message (%d)", te.Code))
		}
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) {
		return channel.Permanent("tls_failed", "the SMTP server's certificate is not valid for its host")
	}
	return netError(err)
}

func smtpRcptError(err error) *channel.SendError {
	var te *textproto.Error
	if errors.As(err, &te) && (te.Code == 550 || te.Code == 551 || te.Code == 553) {
		return channel.Permanent("invalid_recipient", fmt.Sprintf("the SMTP server refused the recipient (%d)", te.Code))
	}
	return smtpError(err)
}
