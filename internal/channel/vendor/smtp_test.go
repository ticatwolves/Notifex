package vendor_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
)

const smtpHost = "smtp.acme.test"

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: smtpHost}, DNSNames: []string{smtpHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}, pool
}

// fakeSMTP is a minimal ESMTP server: EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, QUIT.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	implicit bool
	noTLS    bool // don't offer STARTTLS
	authCode int
	rcptCode int

	mu                         sync.Mutex
	from, to, data, user, pass string
	tlsUsed                    bool
	sawMail                    bool
}

func newFakeSMTP(t *testing.T, cert tls.Certificate, mut func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeSMTP{ln: ln, cert: cert, authCode: 235, rcptCode: 250}
	if mut != nil {
		mut(f)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	conf := &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12}
	secure := false
	if f.implicit {
		c = tls.Server(c, conf)
		secure = true
	}
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			lines := []string{"fake"}
			if !secure && !f.noTLS {
				lines = append(lines, "STARTTLS")
			}
			if secure {
				lines = append(lines, "AUTH PLAIN")
			}
			lines = append(lines, "8BITMIME")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			tc := tls.Server(c, conf)
			if tc.Handshake() != nil {
				return
			}
			c, secure = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			parts := strings.Fields(line)
			if len(parts) == 3 {
				raw, _ := base64.StdEncoding.DecodeString(parts[2])
				up := strings.Split(string(raw), "\x00")
				if len(up) == 3 {
					f.mu.Lock()
					f.user, f.pass = up[1], up[2]
					f.mu.Unlock()
				}
			}
			_ = tp.PrintfLine("%d auth", f.authCode)
		case "MAIL":
			f.mu.Lock()
			f.sawMail, f.from, f.tlsUsed = true, line, secure
			f.mu.Unlock()
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			f.mu.Lock()
			f.to = line
			f.mu.Unlock()
			_ = tp.PrintfLine("%d rcpt", f.rcptCode)
		case "DATA":
			_ = tp.PrintfLine("354 go")
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.data = string(b)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("502 nope")
		}
	}
}

func smtpSender(f *fakeSMTP, pool *x509.CertPool, dialed *[]string) *vendor.SMTP {
	s := vendor.NewSMTP(nil, "notifex.test")
	s.RootCAs = pool
	s.Timeout = 5 * time.Second
	s.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		if dialed != nil {
			*dialed = append(*dialed, addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", f.ln.Addr().String())
	}
	return s
}

func smtpCred(port int, mode string) channel.Credential {
	return channel.Credential{Mode: "byo", Provider: "smtp",
		Config: map[string]any{"host": smtpHost, "port": float64(port), "tls": mode, "username": "mailer", "from_email": "orders@acme.test", "from_name": "Acme Orders"},
		Secret: map[string]any{"password": "pw-123"}}
}

func TestSMTPStartTLS(t *testing.T) {
	t.Parallel()
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert, nil)
	var dialed []string
	s := smtpSender(f, pool, &dialed)
	m := msg("email")
	m.Email.Subject = "Commande №A1 confirmée"
	m.ReplyTo = "help@acme.test"
	res, err := s.Send(context.Background(), m, smtpCred(587, "starttls"))
	require.NoError(t, err)
	assert.Equal(t, m.NotificationID.String()+"@notifex", res.VendorMessageID)
	assert.False(t, res.Delivered)
	assert.Equal(t, []string{smtpHost + ":587"}, dialed)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.True(t, f.tlsUsed, "upgraded before MAIL")
	assert.Equal(t, "mailer", f.user)
	assert.Equal(t, "pw-123", f.pass)
	assert.Equal(t, "MAIL FROM:<orders@acme.test> BODY=8BITMIME", f.from)
	assert.Equal(t, "RCPT TO:<user@example.com>", f.to)
	msgText := f.data
	assert.Contains(t, msgText, "From: \"Acme Orders\" <orders@acme.test>\n")
	assert.Contains(t, msgText, "To: <user@example.com>\n")
	assert.Contains(t, msgText, "Reply-To: <help@acme.test>\n")
	assert.Contains(t, msgText, "Subject: =?utf-8?q?", "non-ASCII subject is RFC 2047-encoded")
	assert.Contains(t, msgText, "Message-ID: <"+m.NotificationID.String()+"@notifex>")
	assert.Contains(t, msgText, "multipart/alternative")
	assert.Contains(t, msgText, "text/plain; charset=utf-8")
	assert.Contains(t, msgText, "<p>Hi</p>")
}

func TestSMTPImplicitTLS(t *testing.T) {
	t.Parallel()
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert, func(f *fakeSMTP) { f.implicit = true })
	_, err := smtpSender(f, pool, nil).Send(context.Background(), msg("email"), smtpCred(465, "implicit"))
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.True(t, f.tlsUsed)
}

func TestSMTPFailures(t *testing.T) {
	t.Parallel()
	cert, pool := selfSigned(t)
	for name, c := range map[string]struct {
		mut       func(*fakeSMTP)
		pool      *x509.CertPool
		code      string
		retryable bool
		mailSent  bool
	}{
		"no STARTTLS offered: never plaintext": {func(f *fakeSMTP) { f.noTLS = true }, pool, "tls_unavailable", false, false},
		"untrusted certificate":                {nil, x509.NewCertPool(), "tls_failed", false, false},
		"bad login":                            {func(f *fakeSMTP) { f.authCode = 535 }, pool, "auth_failed", false, false},
		"unknown mailbox":                      {func(f *fakeSMTP) { f.rcptCode = 550 }, pool, "invalid_recipient", false, true},
		"greylisted":                           {func(f *fakeSMTP) { f.rcptCode = 451 }, pool, "smtp_temporary", true, true},
	} {
		f := newFakeSMTP(t, cert, c.mut)
		_, err := smtpSender(f, c.pool, nil).Send(context.Background(), msg("email"), smtpCred(587, "starttls"))
		se := classify(t, err)
		assert.Equal(t, c.code, se.Code, name)
		assert.Equal(t, c.retryable, se.Retryable, name)
		f.mu.Lock()
		assert.Equal(t, c.mailSent, f.sawMail, name)
		assert.Empty(t, f.data, "%s: no message transferred", name)
		f.mu.Unlock()
	}
}

func TestSMTPRefusesBeforeDialing(t *testing.T) {
	t.Parallel()
	var dialed []string
	s := smtpSender(&fakeSMTP{ln: nopListener{}}, nil, &dialed)
	m := msg("email")
	m.Email.Subject = "Hi\r\nBcc: victim@x.test"
	_, err := s.Send(context.Background(), m, smtpCred(587, "starttls"))
	assert.Equal(t, "invalid_content", classify(t, err).Code, "header injection")

	for _, host := range []string{"localhost", "10.0.0.5", "mail.internal", "169.254.169.254"} {
		cred := smtpCred(587, "starttls")
		cred.Config["host"] = host
		_, err := s.Send(context.Background(), msg("email"), cred)
		assert.Equal(t, "credential_invalid", classify(t, err).Code, host)
	}
	cred := smtpCred(587, "none")
	_, err = s.Send(context.Background(), msg("email"), cred)
	assert.Equal(t, "credential_invalid", classify(t, err).Code, "no plaintext mode")
	assert.Empty(t, dialed)
}

type nopListener struct{ net.Listener }

func (nopListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
