package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// Sealed is a small secret encrypted under its own data key (envelope
// encryption for platform secrets such as operator TOTP seeds). It is not
// tied to any tenant DEK: crypto-shredding a tenant never affects it.
type Sealed struct {
	WrappedKey []byte
	KeyID      string
	Ciphertext []byte // nonce(12) | AES-256-GCM(ciphertext+tag)
}

// Seal encrypts plaintext under a fresh data key from kp. ec is bound to
// the wrapped key and also used as AEAD additional data, so a Sealed value
// copied to another record (another ec) does not open.
func Seal(ctx context.Context, kp KeyProvider, ec EncryptionContext, plaintext []byte) (Sealed, error) {
	dk, err := kp.GenerateDataKey(ctx, ec)
	if err != nil {
		return Sealed{}, fmt.Errorf("crypto: seal: %w", err)
	}
	defer dk.Zero()
	aead, err := gcm(dk.Plaintext)
	if err != nil {
		return Sealed{}, err
	}
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+tagSize)
	if _, err := rand.Read(out); err != nil {
		return Sealed{}, fmt.Errorf("crypto: seal nonce: %w", err)
	}
	return Sealed{
		WrappedKey: dk.Wrapped, KeyID: dk.KeyID,
		Ciphertext: aead.Seal(out, out[:nonceSize], plaintext, ec.canonical()),
	}, nil
}

// Open decrypts s with the same encryption context used to seal it.
func Open(ctx context.Context, kp KeyProvider, ec EncryptionContext, s Sealed) ([]byte, error) {
	if len(s.Ciphertext) < nonceSize+tagSize {
		return nil, ErrDecrypt
	}
	key, err := kp.Unwrap(ctx, s.WrappedKey, s.KeyID, ec)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: %w", err)
	}
	defer clear(key)
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, s.Ciphertext[:nonceSize], s.Ciphertext[nonceSize:], ec.canonical())
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher: %w", err)
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	return a, nil
}
