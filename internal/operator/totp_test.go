package operator

import (
	"encoding/base32"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// RFC 6238 Appendix B test vectors (SHA1, 8 digits, seed "12345678901234567890").
func TestTOTPRFC6238Vectors(t *testing.T) {
	seed := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	} {
		assert.Equal(t, want, totpCode(seed, stepAt(time.Unix(unix, 0)), 8), "T=%d", unix)
	}
}

func TestMatchStepAcceptsSkewOnly(t *testing.T) {
	secret := newTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	cur := stepAt(now)
	for d, ok := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		code := totpCode(secret, cur+d, totpDigits)
		got := matchStep(secret, code, now)
		if ok {
			assert.Equal(t, cur+d, got, "skew %d accepted", d)
		} else {
			assert.Equal(t, int64(-1), got, "skew %d rejected", d)
		}
	}
	assert.Equal(t, int64(-1), matchStep(secret, "12345", now), "wrong length")
	assert.Equal(t, int64(-1), matchStep(secret, "abcdef", now))
}

func TestOTPAuthURI(t *testing.T) {
	secret := []byte("12345678901234567890") // RFC 6238 test key
	u := otpauthURI(secret, "ops@notifex.dev")
	assert.Contains(t, u, "otpauth://totp/Notifex%20Operator:ops@notifex.dev?")
	// Unpadded base32 of the key (computed, so no key-shaped literal in the repo).
	assert.Contains(t, u, "secret="+base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret))
	assert.Contains(t, u, "digits=6")
}
