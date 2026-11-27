package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLocal(t testing.TB) *Local {
	t.Helper()
	key := make([]byte, DEKSize)
	_, _ = rand.Read(key)
	l, err := NewLocal(key)
	require.NoError(t, err)
	return l
}

func TestNewLocalValidatesKey(t *testing.T) {
	_, err := NewLocal(make([]byte, 16))
	require.Error(t, err)

	key := bytes.Repeat([]byte{7}, DEKSize)
	for name, s := range map[string]string{
		"std":     base64.StdEncoding.EncodeToString(key),
		"url":     base64.URLEncoding.EncodeToString(key),
		"raw std": base64.RawStdEncoding.EncodeToString(key),
	} {
		l, err := NewLocalFromBase64(s)
		require.NoError(t, err, name)
		assert.Contains(t, l.KeyID(), "local:")
	}
	_, err = NewLocalFromBase64("not base64!!")
	require.Error(t, err)
	_, err = NewLocalFromBase64(base64.StdEncoding.EncodeToString(key[:31]))
	require.Error(t, err)
}

func TestLocalWrapUnwrap(t *testing.T) {
	ctx := context.Background()
	l := newTestLocal(t)
	ec := TenantContext("tenant-a")

	dk, err := l.GenerateDataKey(ctx, ec)
	require.NoError(t, err)
	require.Len(t, dk.Plaintext, DEKSize)
	assert.Equal(t, l.KeyID(), dk.KeyID)
	assert.NotContains(t, string(dk.Wrapped), string(dk.Plaintext), "wrapped key must not contain the plaintext")

	got, err := l.Unwrap(ctx, dk.Wrapped, dk.KeyID, ec)
	require.NoError(t, err)
	assert.Equal(t, dk.Plaintext, got)

	dk2, err := l.GenerateDataKey(ctx, ec)
	require.NoError(t, err)
	assert.NotEqual(t, dk.Plaintext, dk2.Plaintext, "each DEK is fresh")

	plain := bytes.Clone(dk.Plaintext)
	dk.Zero()
	assert.Equal(t, make([]byte, DEKSize), dk.Plaintext)
	assert.NotEqual(t, plain, dk.Plaintext)
}

func TestLocalUnwrapFailures(t *testing.T) {
	ctx := context.Background()
	l := newTestLocal(t)
	other := newTestLocal(t)
	ec := TenantContext("tenant-a")
	dk, err := l.GenerateDataKey(ctx, ec)
	require.NoError(t, err)

	flip := func(i int) []byte { b := bytes.Clone(dk.Wrapped); b[i] ^= 1; return b }

	tests := []struct {
		name    string
		wrapped []byte
		keyID   string
		ec      EncryptionContext
		target  error
	}{
		{name: "other tenant's context", wrapped: dk.Wrapped, keyID: dk.KeyID, ec: TenantContext("tenant-b"), target: ErrDecrypt},
		{name: "extra context key", wrapped: dk.Wrapped, keyID: dk.KeyID,
			ec: EncryptionContext{"purpose": "tenant-dek", "tenant_id": "tenant-a", "x": "y"}, target: ErrDecrypt},
		{name: "empty context", wrapped: dk.Wrapped, keyID: dk.KeyID, ec: nil, target: ErrDecrypt},
		{name: "version byte flipped", wrapped: flip(0), keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "nonce flipped", wrapped: flip(3), keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "ciphertext flipped", wrapped: flip(20), keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "tag flipped", wrapped: flip(len(dk.Wrapped) - 1), keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "truncated", wrapped: dk.Wrapped[:len(dk.Wrapped)-1], keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "empty", wrapped: nil, keyID: dk.KeyID, ec: ec, target: ErrDecrypt},
		{name: "unknown key id", wrapped: dk.Wrapped, keyID: other.KeyID(), ec: ec, target: ErrUnknownKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := l.Unwrap(ctx, tt.wrapped, tt.keyID, tt.ec)
			require.ErrorIs(t, err, tt.target)
		})
	}

	// A different master key that is told the right key id still can't unwrap.
	forged := &Local{aead: other.aead, keyID: l.keyID}
	_, err = forged.Unwrap(ctx, dk.Wrapped, dk.KeyID, ec)
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestEncryptionContextCanonicalIsUnambiguous(t *testing.T) {
	// Length prefixes stop "ab"+"c" colliding with "a"+"bc".
	a := EncryptionContext{"ab": "c"}.canonical()
	b := EncryptionContext{"a": "bc"}.canonical()
	assert.NotEqual(t, a, b)
	// Order-independent.
	x := EncryptionContext{"k1": "v1", "k2": "v2"}.canonical()
	y := EncryptionContext{"k2": "v2", "k1": "v1"}.canonical()
	assert.Equal(t, x, y)
}
