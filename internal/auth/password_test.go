package auth

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
)

func TestPasswordHashVerify(t *testing.T) {
	ctx := context.Background()
	h := NewPasswordHasher(2)
	enc, err := h.Hash(ctx, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(enc, "$argon2id$v=19$m=47104,t=1,p=1$"))

	ok, rehash, err := h.Verify(ctx, enc, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, rehash)

	ok, _, err = h.Verify(ctx, enc, "correct horse batterY")
	require.NoError(t, err)
	assert.False(t, ok)

	enc2, err := h.Hash(ctx, "correct horse battery")
	require.NoError(t, err)
	assert.NotEqual(t, enc, enc2, "random salt per hash")
}

func TestPasswordNeedsRehashOnOldParams(t *testing.T) {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	old := encodePHC(salt, argon2.IDKey([]byte("legacy password!"), salt, 2, 19456, 1, 32), 19456, 2, 1)
	ok, rehash, err := NewPasswordHasher(1).Verify(context.Background(), old, "legacy password!")
	require.NoError(t, err)
	assert.True(t, ok, "old parameters still verify")
	assert.True(t, rehash, "and are flagged for upgrade")
}

func TestPasswordRejectsMalformedOrHostileHashes(t *testing.T) {
	h := NewPasswordHasher(1)
	for name, enc := range map[string]string{
		"empty":              "",
		"bcrypt":             "$2a$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz012345",
		"argon2i":            "$argon2i$v=19$m=47104,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"wrong version":      "$argon2id$v=16$m=47104,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"1 TiB memory (DoS)": "$argon2id$v=19$m=1073741824,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"huge time (DoS)":    "$argon2id$v=19$m=47104,t=1000,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"zero threads":       "$argon2id$v=19$m=47104,t=1,p=0$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"bad base64":         "$argon2id$v=19$m=47104,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"short key":          "$argon2id$v=19$m=47104,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
	} {
		t.Run(name, func(t *testing.T) {
			ok, _, err := h.Verify(context.Background(), enc, "whatever password")
			require.Error(t, err)
			assert.False(t, ok)
		})
	}
}

func TestPasswordHasherBoundsConcurrency(t *testing.T) {
	h := NewPasswordHasher(1)
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := h.Hash(ctx, "some long password")
	require.ErrorIs(t, err, context.DeadlineExceeded, "waits for a slot instead of allocating another 46 MiB")
	<-h.sem
}

func TestValidatePassword(t *testing.T) {
	tests := map[string]struct {
		pw, email string
		ok        bool
	}{
		"good passphrase":          {"plum-orbit-candle-91", "ada@example.com", true},
		"unicode counts runes":     {"पासवर्ड-मज़बूत-है", "ada@example.com", true},
		"too short":                {"Sh0rt!pw", "ada@example.com", false},
		"exactly 12":               {"k7#Rm2!pQ9zX", "ada@example.com", true},
		"too long":                 {strings.Repeat("ab1!", 33), "ada@example.com", false},
		"common":                   {"password1234", "ada@example.com", false},
		"common, case-insensitive": {"PASSWORD1234", "ada@example.com", false},
		"repeated chars":           {"aaaaaaaaaaaaaa", "ada@example.com", false},
		"few distinct":             {"abababababab", "ada@example.com", false},
		"sequential":               {"abcdefghijklm", "ada@example.com", false},
		"reverse sequential":       {"987654321098"[:10] + "xx", "ada@example.com", true}, // broken run is fine
		"contains email local":     {"lovelace-rocks-2026", "lovelace@example.com", false},
		"short local part ignored": {"ada-plum-orbit-91", "ada@example.com", true},
		"product name":             {"my-notifex-secret-1", "ada@example.com", false},
		"invalid utf8":             {"\xff\xfe" + strings.Repeat("z", 12), "ada@example.com", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidatePassword(tt.pw, tt.email)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrWeakPassword)
			}
		})
	}
}

func TestValidateRedirectURIs(t *testing.T) {
	tests := map[string]struct {
		uris []string
		ok   bool
	}{
		"https":                {[]string{"https://console.notifex.dev/oauth/callback"}, true},
		"localhost http":       {[]string{"http://localhost:3000/oauth/callback", "http://127.0.0.1:3000/cb"}, true},
		"none":                 {nil, true},
		"plain http elsewhere": {[]string{"http://console.notifex.dev/cb"}, false},
		"relative":             {[]string{"/oauth/callback"}, false},
		"fragment":             {[]string{"https://console.notifex.dev/cb#x"}, false},
		"javascript scheme":    {[]string{"javascript:alert(1)"}, false},
		"one bad among good":   {[]string{"https://a.dev/cb", "ftp://a.dev/cb"}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateRedirectURIs(tt.uris)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
