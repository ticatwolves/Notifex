package auth

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keyRE = regexp.MustCompile(`^nfx_(dev|staging|prod)_[a-z0-9]{12}_[0-9A-Za-z]{49}$`)

func testHasher(t testing.TB) *Hasher {
	t.Helper()
	h, err := NewHasher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", 32))))
	require.NoError(t, err)
	return h
}

// withChecksum rebuilds a key from parts with a valid checksum, for crafting
// keys that pass Parse but are wrong in some other way.
func withChecksum(env, prefix, secret string) string {
	body := keyScheme + "_" + env + "_" + prefix + "_" + secret
	return body + checksum(body)
}

func TestGenerateParseRoundTrip(t *testing.T) {
	seen := map[string]bool{}
	for _, env := range []string{"dev", "staging", "prod"} {
		for range 50 {
			key, parsed, err := Generate(env)
			require.NoError(t, err)
			assert.Regexp(t, keyRE, key)
			assert.False(t, seen[key], "keys are unique")
			seen[key] = true

			got, err := Parse(key)
			require.NoError(t, err)
			assert.Equal(t, parsed, got)
			assert.Equal(t, env, got.Env)
		}
	}
	_, _, err := Generate("qa")
	require.Error(t, err)
}

func TestParseRejectsMalformed(t *testing.T) {
	key, p, err := Generate("prod")
	require.NoError(t, err)
	tests := map[string]string{
		"empty":            "",
		"wrong scheme":     "abc" + key[3:],
		"unknown env":      strings.Replace(key, "_prod_", "_live_", 1),
		"missing part":     strings.Join(strings.Split(key, "_")[:3], "_"),
		"extra underscore": key + "_x",
		"short prefix":     withChecksum("prod", p.Prefix[:11], p.secret),
		"upper in prefix":  withChecksum("prod", "A"+p.Prefix[1:], p.secret), // always a letter; a digit has no upper case
		"short secret":     withChecksum("prod", p.Prefix, p.secret[:42]),
		"bad char":         withChecksum("prod", p.Prefix, p.secret[:42]+"-"),
		"too long":         key + strings.Repeat("a", 100),
		"whitespace":       " " + key,
	}
	for name, k := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(k)
			require.ErrorIs(t, err, ErrMalformedKey)
		})
	}
}

func TestChecksumCatchesEverySingleCharTypo(t *testing.T) {
	key, _, err := Generate("dev")
	require.NoError(t, err)
	start := len("nfx_dev_")
	for i := start; i < len(key); i++ {
		if key[i] == '_' {
			continue
		}
		b := []byte(key)
		if b[i] == 'a' {
			b[i] = 'b'
		} else {
			b[i] = 'a'
		}
		_, err := Parse(string(b))
		require.ErrorIs(t, err, ErrMalformedKey, "typo at %d not caught", i)
	}
}

func TestHasher(t *testing.T) {
	_, err := NewHasher("not base64!")
	require.Error(t, err)
	_, err = NewHasher(base64.StdEncoding.EncodeToString(make([]byte, 31)))
	require.Error(t, err, "pepper shorter than 32 bytes")

	h := testHasher(t)
	_, p, err := Generate("prod")
	require.NoError(t, err)
	stored := h.Hash(p)
	assert.Len(t, stored, 32)
	assert.True(t, h.Verify(p, stored))

	_, other, err := Generate("prod")
	require.NoError(t, err)
	assert.False(t, h.Verify(other, stored), "different secret")

	moved := p
	moved.Prefix = other.Prefix
	assert.False(t, h.Verify(moved, stored), "hash binds the prefix: a secret can't be replayed under another prefix")

	h2, err := NewHasher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("q", 32))))
	require.NoError(t, err)
	assert.False(t, h2.Verify(p, stored), "different pepper")
	assert.NotContains(t, string(stored), p.secret)
}
