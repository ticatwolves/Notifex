// Package auth authenticates callers. This file defines the API key format
// and its keyed hash.
//
// Format: nfx_<env>_<prefix>_<secret><checksum>
//
//	env       dev | staging | prod (must match the key's environment)
//	prefix    12 chars [a-z0-9], unique, the public lookup handle
//	secret    43 chars base62 (256 random bits)
//	checksum  6 chars base62 CRC32 of everything before it
//
// The checksum rejects typos and garbage before any database query and gives
// secret scanners a self-validating pattern. Only HMAC-SHA256(pepper, …) of
// the prefix and secret is stored; the full key is shown once at creation.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
)

const (
	keyScheme      = "nfx"
	prefixLen      = 12
	secretLen      = 43 // ceil(256 / log2(62))
	checksumLen    = 6  // ceil(32 / log2(62))
	minPepperBytes = 32
	hashDomain     = "nfx-api-key-v1\x00"
	prefixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// ErrMalformedKey means the string is not a well-formed Notifex API key.
var ErrMalformedKey = errors.New("auth: malformed api key")

var validEnvs = map[string]bool{"dev": true, "staging": true, "prod": true}

// ParsedKey is a syntactically valid key, split into its parts.
type ParsedKey struct {
	Env    string
	Prefix string
	secret string
}

// Hasher computes the stored hash of API keys under a server-side pepper.
type Hasher struct{ pepper []byte }

// NewHasher builds a Hasher from a base64-encoded pepper of at least 32 bytes.
func NewHasher(pepperB64 string) (*Hasher, error) {
	p, err := base64.StdEncoding.DecodeString(pepperB64)
	if err != nil {
		return nil, errors.New("auth: API_KEY_PEPPER must be base64")
	}
	if len(p) < minPepperBytes {
		return nil, fmt.Errorf("auth: API_KEY_PEPPER must decode to at least %d bytes", minPepperBytes)
	}
	return &Hasher{pepper: p}, nil
}

// Hash returns HMAC-SHA256(pepper, domain | prefix | ":" | secret).
func (h *Hasher) Hash(k ParsedKey) []byte {
	m := hmac.New(sha256.New, h.pepper)
	m.Write([]byte(hashDomain))
	m.Write([]byte(k.Prefix))
	m.Write([]byte{':'})
	m.Write([]byte(k.secret))
	return m.Sum(nil)
}

// Verify reports whether k matches stored, in constant time.
func (h *Hasher) Verify(k ParsedKey, stored []byte) bool {
	return hmac.Equal(h.Hash(k), stored)
}

// Generate returns a new key for env as plaintext (show once) and parsed form.
func Generate(env string) (string, ParsedKey, error) {
	if !validEnvs[env] {
		return "", ParsedKey{}, fmt.Errorf("auth: unknown environment %q", env)
	}
	prefix, err := randomString(prefixLen, prefixAlphabet)
	if err != nil {
		return "", ParsedKey{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", ParsedKey{}, fmt.Errorf("auth: generate secret: %w", err)
	}
	secret := base62(raw, secretLen)
	body := keyScheme + "_" + env + "_" + prefix + "_" + secret
	return body + checksum(body), ParsedKey{Env: env, Prefix: prefix, secret: secret}, nil
}

// Parse validates the format and checksum without touching storage.
func Parse(key string) (ParsedKey, error) {
	const maxLen = len(keyScheme) + 1 + len("staging") + 1 + prefixLen + 1 + secretLen + checksumLen
	if len(key) > maxLen {
		return ParsedKey{}, ErrMalformedKey
	}
	parts := strings.Split(key, "_")
	if len(parts) != 4 || parts[0] != keyScheme || !validEnvs[parts[1]] {
		return ParsedKey{}, ErrMalformedKey
	}
	prefix, tail := parts[2], parts[3]
	if len(prefix) != prefixLen || !only(prefix, prefixAlphabet) ||
		len(tail) != secretLen+checksumLen || !only(tail, base62Alphabet) {
		return ParsedKey{}, ErrMalformedKey
	}
	secret, sum := tail[:secretLen], tail[secretLen:]
	body := key[:len(key)-checksumLen]
	if !hmac.Equal([]byte(checksum(body)), []byte(sum)) {
		return ParsedKey{}, ErrMalformedKey
	}
	return ParsedKey{Env: parts[1], Prefix: prefix, secret: secret}, nil
}

const base62Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// base62 encodes b as exactly n base62 digits (left-padded with '0').
func base62(b []byte, n int) string {
	s := new(big.Int).SetBytes(b).Text(62) // big.Int uses 0-9a-zA-Z for base 62
	if len(s) < n {
		s = strings.Repeat("0", n-len(s)) + s
	}
	return s
}

func checksum(body string) string {
	return base62(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE([]byte(body))), checksumLen)
}

func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("auth: random: %w", err)
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}

func only(s, alphabet string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}
