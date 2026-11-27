// Package operator implements the platform operator realm (ADR-011 §5):
// CLI-provisioned accounts, password + TOTP login, separate sessions, and
// audited tenant administration.
package operator

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// TOTP parameters (RFC 6238 defaults; what authenticator apps support).
const (
	totpStep   = 30 * time.Second
	totpDigits = 6
	totpSkew   = 1 // accept codes one step either side (clock drift)
	secretLen  = 20
)

// newTOTPSecret returns a random 160-bit secret.
func newTOTPSecret() []byte {
	b := make([]byte, secretLen)
	_, _ = rand.Read(b)
	return b
}

// totpCode computes the code for a time step (RFC 4226 dynamic truncation).
func totpCode(secret []byte, step int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // G115: steps are positive
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// stepAt is the TOTP time step containing t.
func stepAt(t time.Time) int64 { return t.Unix() / int64(totpStep/time.Second) }

// matchStep returns the step whose code equals code within ±totpSkew of now,
// or -1. Every candidate is compared in constant time.
func matchStep(secret []byte, code string, now time.Time) int64 {
	if len(code) != totpDigits {
		return -1
	}
	found := int64(-1)
	cur := stepAt(now)
	for d := -totpSkew; d <= totpSkew; d++ {
		s := cur + int64(d)
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s, totpDigits)), []byte(code)) == 1 {
			found = s
		}
	}
	return found
}

// otpauthURI is the enrollment URI for authenticator apps (shown once).
func otpauthURI(secret []byte, email string) string {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	q := url.Values{}
	q.Set("secret", enc)
	q.Set("issuer", "Notifex Operator")
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("Notifex Operator:"+email) + "?" + q.Encode()
}

// CodeFromURI computes the code an authenticator app enrolled with uri shows
// at t. Used by tests and local tooling; it needs the secret, so it grants
// nothing an enrolled app doesn't already have.
func CodeFromURI(uri string, t time.Time) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("totp uri: %w", err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(u.Query().Get("secret"))
	if err != nil || len(secret) == 0 {
		return "", fmt.Errorf("totp uri: bad secret")
	}
	return totpCode(secret, stepAt(t), totpDigits), nil
}
