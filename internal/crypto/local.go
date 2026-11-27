package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const localWrapVersion byte = 1

// Local is a KeyProvider backed by a 256-bit master key held in process
// memory. It is for local development and tests only: the master key comes
// from an env var, so anyone with the environment can unwrap every DEK.
// Deployed environments use KMS.
type Local struct {
	aead  cipher.AEAD
	keyID string
}

var _ KeyProvider = (*Local)(nil)

// NewLocal builds a Local provider from a 32-byte master key.
func NewLocal(masterKey []byte) (*Local, error) {
	if len(masterKey) != DEKSize {
		return nil, fmt.Errorf("local master key must be %d bytes, got %d", DEKSize, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("local master key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("local master key: %w", err)
	}
	// The key id is a fingerprint, so a DEK wrapped under a different local key
	// fails with ErrUnknownKey instead of a generic auth failure.
	fp := sha256.Sum256(append([]byte("notifex-local-master-key-id\x00"), masterKey...))
	return &Local{aead: aead, keyID: "local:" + hex.EncodeToString(fp[:8])}, nil
}

// NewLocalFromBase64 parses a base64 (std or URL encoding) 32-byte master key,
// e.g. the value of an env var generated with `openssl rand -base64 32`.
func NewLocalFromBase64(s string) (*Local, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			defer clear(key)
			return NewLocal(key)
		}
	}
	return nil, errors.New("local master key: not valid base64")
}

// KeyID returns the master key fingerprint stored alongside wrapped DEKs.
func (l *Local) KeyID() string { return l.keyID }

// GenerateDataKey implements KeyProvider.
func (l *Local) GenerateDataKey(_ context.Context, ec EncryptionContext) (DataKey, error) {
	dek := make([]byte, DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return DataKey{}, fmt.Errorf("generate dek: %w", err)
	}
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		clear(dek)
		return DataKey{}, fmt.Errorf("generate nonce: %w", err)
	}
	// wrapped = version | nonce | AES-GCM(master, dek, aad = version | canonical(ec))
	wrapped := make([]byte, 0, 1+len(nonce)+DEKSize+l.aead.Overhead())
	wrapped = append(wrapped, localWrapVersion)
	wrapped = append(wrapped, nonce...)
	wrapped = l.aead.Seal(wrapped, nonce, dek, l.aad(ec))
	return DataKey{Plaintext: dek, Wrapped: wrapped, KeyID: l.keyID}, nil
}

// Unwrap implements KeyProvider.
func (l *Local) Unwrap(_ context.Context, wrapped []byte, keyID string, ec EncryptionContext) ([]byte, error) {
	if keyID != l.keyID {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, keyID)
	}
	ns := l.aead.NonceSize()
	if len(wrapped) != 1+ns+DEKSize+l.aead.Overhead() || wrapped[0] != localWrapVersion {
		return nil, ErrDecrypt
	}
	dek, err := l.aead.Open(nil, wrapped[1:1+ns], wrapped[1+ns:], l.aad(ec))
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

func (l *Local) aad(ec EncryptionContext) []byte {
	return append([]byte{localWrapVersion}, ec.canonical()...)
}
