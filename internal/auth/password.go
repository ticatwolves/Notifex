package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: OWASP Password Storage Cheat Sheet (m=46 MiB, t=1, p=1).
// They are encoded in every hash (PHC format), so raising them later is a
// config change: old hashes still verify and are re-hashed on next login.
const (
	argonMemoryKiB = 47104
	argonTime      = 1
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16

	// Bounds on parameters accepted from stored hashes.
	maxArgonMemoryKiB = 1 << 20 // 1 GiB
	maxArgonTime      = 10
)

// Password length bounds (ADR-011 §2). Length is counted in runes.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 128
)

// ErrWeakPassword means a new password fails the policy.
var ErrWeakPassword = errors.New("auth: password does not meet the policy")

// PasswordHasher hashes and verifies passwords with argon2id. Each hash uses
// ~46 MiB, so concurrent hashing is bounded to keep a login flood from
// exhausting memory on a small task.
type PasswordHasher struct {
	sem   chan struct{}
	dummy string
}

// NewPasswordHasher allows up to concurrency hashes at once (default 4).
func NewPasswordHasher(concurrency int) *PasswordHasher {
	if concurrency <= 0 {
		concurrency = 4
	}
	h := &PasswordHasher{sem: make(chan struct{}, concurrency)}
	// A real hash of a random password, verified against when the user is
	// unknown so response time doesn't reveal whether an account exists.
	salt := make([]byte, argonSaltLen)
	pw := make([]byte, 32)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(pw)
	h.dummy = encodePHC(salt, argon2.IDKey(pw, salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen), argonMemoryKiB, argonTime, argonThreads)
	return h
}

func (h *PasswordHasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("auth: password hasher busy: %w", ctx.Err())
	}
}

func (h *PasswordHasher) release() { <-h.sem }

// Hash returns a PHC-encoded argon2id hash.
func (h *PasswordHasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return encodePHC(salt, key, argonMemoryKiB, argonTime, argonThreads), nil
}

// Verify checks password against an encoded hash in constant time.
// needsRehash reports that the hash uses outdated parameters.
func (h *PasswordHasher) Verify(ctx context.Context, encoded, password string) (ok, needsRehash bool, err error) {
	p, err := decodePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key))) //nolint:gosec // G115: key length bounded by decodePHC
	ok = subtle.ConstantTimeCompare(key, p.key) == 1
	needsRehash = p.memory != argonMemoryKiB || p.time != argonTime || p.threads != argonThreads || len(p.key) != argonKeyLen
	return ok, needsRehash, nil
}

// VerifyDummy spends the same work as Verify against a hash that never
// matches. Call it when the account does not exist.
func (h *PasswordHasher) VerifyDummy(ctx context.Context, password string) {
	_, _, _ = h.Verify(ctx, h.dummy, password)
}

type phc struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

func encodePHC(salt, key []byte, m, t uint32, p uint8) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, m, t, p, b64.EncodeToString(salt), b64.EncodeToString(key))
}

var errBadHash = errors.New("auth: malformed password hash")

func decodePHC(s string) (phc, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return phc{}, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return phc{}, errBadHash
	}
	var p phc
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return phc{}, errBadHash
	}
	if p.memory < 8 || p.memory > maxArgonMemoryKiB || p.time < 1 || p.time > maxArgonTime || p.threads < 1 {
		return phc{}, errBadHash
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return phc{}, errBadHash
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < 16 || len(p.key) > 64 {
		return phc{}, errBadHash
	}
	return p, nil
}

// ValidatePassword enforces the policy for a new password (ADR-011 §2):
// 12–128 characters, not a known-common password, not a trivial pattern,
// and not containing the account's email local part.
func ValidatePassword(password, email string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case !utf8.ValidString(password):
		return fmt.Errorf("%w: must be valid UTF-8", ErrWeakPassword)
	case n < MinPasswordLen:
		return fmt.Errorf("%w: at least %d characters", ErrWeakPassword, MinPasswordLen)
	case n > MaxPasswordLen:
		return fmt.Errorf("%w: at most %d characters", ErrWeakPassword, MaxPasswordLen)
	}
	lower := strings.ToLower(password)
	if commonPasswords[lower] {
		return fmt.Errorf("%w: too common", ErrWeakPassword)
	}
	if distinctRunes(lower) <= 3 || isSequential(lower) {
		return fmt.Errorf("%w: too predictable", ErrWeakPassword)
	}
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok && len(local) >= 4 && strings.Contains(lower, local) {
		return fmt.Errorf("%w: must not contain your email", ErrWeakPassword)
	}
	if strings.Contains(lower, "notifex") {
		return fmt.Errorf("%w: must not contain the product name", ErrWeakPassword)
	}
	return nil
}

func distinctRunes(s string) int {
	seen := map[rune]bool{}
	for _, r := range s {
		seen[r] = true
	}
	return len(seen)
}

// isSequential reports strings that step by +1 or -1 throughout (abcdef…, 987654…).
func isSequential(s string) bool {
	r := []rune(s)
	if len(r) < 2 {
		return false
	}
	step := r[1] - r[0]
	if step != 1 && step != -1 {
		return false
	}
	for i := 2; i < len(r); i++ {
		if r[i]-r[i-1] != step {
			return false
		}
	}
	return true
}

// commonPasswords: frequently breached passwords that satisfy the length rule.
// A curated offline list (no third-party breach API: ADR-011 §2). Extend from
// a vetted dataset before GA.
var commonPasswords = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Fields(`
		123456789012 1234567890123 12345678901234 1234567890qwerty 1q2w3e4r5t6y
		1q2w3e4r5t6y7u 1qaz2wsx3edc 1qaz2wsx3edc4rfv qwertyuiop12 qwertyuiop123
		qwertyuiopasdf qwerty123456 qwerty1234567 qwertyqwerty asdfghjkl123
		zxcvbnm12345 password1234 password12345 password123456 passwordpassword
		password1234! password123! p@ssw0rd1234 p@ssword1234 passw0rd1234
		iloveyou1234 iloveyou12345 letmein12345 welcome12345 welcome123456
		changeme1234 administrator admin1234567 admin12345678 football1234
		baseball1234 superman1234 batman123456 starwars1234 sunshine1234
		princess1234 trustno11234 dragon123456 monkey123456 abc123456789
		aaaaaaaaaaaa 111111111111 000000000000 123123123123 abcabcabcabc
		qazwsxedcrfv q1w2e3r4t5y6 zaq12wsxcde3 1234qwerasdf qwer1234asdf
		thisismypassword mypassword123 mypassword1234 secretpassword
		correcthorsebatterystaple
	`) {
		m[p] = true
	}
	return m
}()
