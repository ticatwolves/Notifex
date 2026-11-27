// Package mail sends Notifex's own system email (verification, password
// reset, invitations). It is not the tenant email channel (Phase 3).
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/ticatwolves/notifex/internal/observability"
)

// Message is a plain-text system email.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Mailer sends a message.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// ErrInvalidMessage means a header field would allow header injection or the
// address does not parse.
var ErrInvalidMessage = errors.New("mail: invalid message")

func validate(from string, m Message) error {
	for _, f := range []string{from, m.To, m.Subject} {
		if strings.ContainsAny(f, "\r\n") {
			return fmt.Errorf("%w: CR/LF in header field", ErrInvalidMessage)
		}
	}
	if _, err := mail.ParseAddress(m.To); err != nil {
		return fmt.Errorf("%w: recipient", ErrInvalidMessage)
	}
	return nil
}

// SMTP sends through a plain SMTP relay. Used with Mailpit locally; deployed
// environments use SES (later), which needs TLS and auth.
type SMTP struct {
	Addr string
	From string
}

// Send implements Mailer.
func (s SMTP) Send(ctx context.Context, m Message) error {
	if err := validate(s.From, m); err != nil {
		return err
	}
	body := "From: " + s.From + "\r\n" +
		"To: " + m.To + "\r\n" +
		"Subject: " + m.Subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"\r\n" + strings.ReplaceAll(m.Text, "\n", "\r\n")

	done := make(chan error, 1)
	go func() {
		addr, _ := mail.ParseAddress(m.To)
		from, _ := mail.ParseAddress(s.From)
		fromAddr := s.From
		if from != nil {
			fromAddr = from.Address
		}
		done <- smtp.SendMail(s.Addr, nil, fromAddr, []string{addr.Address}, []byte(body))
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mail: smtp send: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mail: smtp send: %w", ctx.Err())
	}
}

// Recorder keeps messages in memory (tests, and local runs without SMTP).
type Recorder struct {
	mu   sync.Mutex
	sent []Message
}

// Send implements Mailer.
func (r *Recorder) Send(_ context.Context, m Message) error {
	if err := validate("noreply@notifex.local", m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

// Sent returns a copy of the recorded messages.
func (r *Recorder) Sent() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.sent...)
}

// Last returns the most recent message to addr, if any.
func (r *Recorder) Last(addr string) (Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sent) - 1; i >= 0; i-- {
		if strings.EqualFold(r.sent[i].To, addr) {
			return r.sent[i], true
		}
	}
	return Message{}, false
}

// Queue sends asynchronously through a bounded buffer and worker pool, so
// request latency (and timing) never depends on the mail server. Messages are
// best effort: a full queue or a crash drops them, and users can request a
// new link. Close drains the queue within the context's deadline.
type Queue struct {
	next Mailer
	log  *slog.Logger
	ch   chan Message
	wg   sync.WaitGroup
	once sync.Once
}

// NewQueue starts workers sending through next.
func NewQueue(next Mailer, log *slog.Logger, size, workers int) *Queue {
	q := &Queue{next: next, log: log, ch: make(chan Message, size)}
	for range workers {
		q.wg.Go(q.work)
	}
	return q
}

// Send implements Mailer: it enqueues and returns immediately.
func (q *Queue) Send(_ context.Context, m Message) error {
	if err := validate("noreply@notifex.local", m); err != nil {
		return err
	}
	select {
	case q.ch <- m:
		return nil
	default:
		q.log.Error("system mail queue full; message dropped", slog.String("to", observability.MaskEmail(m.To)))
		return errors.New("mail: queue full")
	}
}

func (q *Queue) work() {
	for m := range q.ch {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := q.next.Send(ctx, m); err != nil {
			q.log.Error("system mail send failed", slog.String("to", observability.MaskEmail(m.To)), slog.Any("error", err))
		}
		cancel()
	}
}

// Close stops accepting messages and waits for queued ones to send.
func (q *Queue) Close(ctx context.Context) error {
	q.once.Do(func() { close(q.ch) })
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mail: drain: %w", ctx.Err())
	}
}
