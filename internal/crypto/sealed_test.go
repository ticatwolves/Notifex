package crypto

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSealOpen(t *testing.T) {
	ctx := context.Background()
	kp := newTestLocal(t)
	ec := EncryptionContext{"purpose": "operator-totp", "operator_id": "op-1"}
	s, err := Seal(ctx, kp, ec, []byte("totp-seed-12345678"))
	require.NoError(t, err)
	assert.False(t, bytes.Contains(s.Ciphertext, []byte("totp-seed")))

	pt, err := Open(ctx, kp, ec, s)
	require.NoError(t, err)
	assert.Equal(t, []byte("totp-seed-12345678"), pt)

	_, err = Open(ctx, kp, EncryptionContext{"purpose": "operator-totp", "operator_id": "op-2"}, s)
	require.ErrorIs(t, err, ErrDecrypt, "bound to its record: another operator's context fails")

	tampered := s
	tampered.Ciphertext = bytes.Clone(s.Ciphertext)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	_, err = Open(ctx, kp, ec, tampered)
	require.ErrorIs(t, err, ErrDecrypt)

	_, err = Open(ctx, newTestLocal(t), ec, s)
	require.ErrorIs(t, err, ErrUnknownKey, "another master key")
	_, err = Open(ctx, kp, ec, Sealed{WrappedKey: s.WrappedKey, KeyID: s.KeyID, Ciphertext: []byte("short")})
	require.ErrorIs(t, err, ErrDecrypt)
}
