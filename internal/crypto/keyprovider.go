// Package crypto implements envelope encryption for sensitive columns
// (ADR-005, ADR-006).
//
// Each tenant has a 256-bit data encryption key (DEK). The DEK is stored only
// in wrapped form: encrypted under a master key (a KMS CMK in AWS, a local key
// in development) by a KeyProvider. Sensitive columns are encrypted with
// AES-256-GCM under a key derived from the tenant's DEK and the column's
// position (tenant, row, column), so a ciphertext copied to another row,
// column or tenant fails to decrypt. Destroying the wrapped DEK makes every
// ciphertext for that tenant unreadable (crypto-shred).
package crypto

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
)

// Errors returned by this package. Callers match with errors.Is.
var (
	// ErrDecrypt means a ciphertext or wrapped key failed authentication: it was
	// tampered with, moved, or encrypted under a different key or context. The
	// error deliberately says nothing more.
	ErrDecrypt = errors.New("crypto: decryption failed")
	// ErrDataKeyDestroyed means the tenant's DEK was crypto-shredded.
	ErrDataKeyDestroyed = errors.New("crypto: tenant data key destroyed")
	// ErrUnknownKey means a wrapped key names a master key this provider does not hold.
	ErrUnknownKey = errors.New("crypto: unknown master key")
	// ErrFormat means a ciphertext has an unsupported format or DEK version.
	ErrFormat = errors.New("crypto: unsupported ciphertext format")
)

// DEKSize is the size of a data encryption key in bytes (AES-256).
const DEKSize = 32

// EncryptionContext is non-secret metadata bound to a wrapped DEK. It must be
// supplied unchanged to unwrap. With KMS it is the KMS encryption context and
// shows up in CloudTrail, so every unwrap is attributable to a tenant. It must
// never contain secrets or PII.
type EncryptionContext map[string]string

// TenantContext returns the standard encryption context for a tenant DEK.
func TenantContext(tenantID string) EncryptionContext {
	return EncryptionContext{"purpose": "tenant-dek", "tenant_id": tenantID}
}

// canonical encodes ec deterministically (sorted, length-prefixed) for use as
// AEAD additional data.
func (ec EncryptionContext) canonical() []byte {
	keys := make([]string, 0, len(ec))
	for k := range ec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = appendLP(b, k)
		b = appendLP(b, ec[k])
	}
	return b
}

func appendLP(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // G115: keys/values are short strings
	return append(b, s...)
}

// DataKey is a freshly generated DEK. Plaintext must be zeroed by the caller
// once used (Zero); only Wrapped and KeyID are persisted.
type DataKey struct {
	Plaintext []byte
	Wrapped   []byte
	KeyID     string
}

// Zero overwrites the plaintext key material.
func (d *DataKey) Zero() { clear(d.Plaintext) }

// KeyProvider wraps and unwraps DEKs under a master key. Implementations: Local
// (dev and tests) and KMS (Phase 8). Implementations must be safe for
// concurrent use.
type KeyProvider interface {
	// GenerateDataKey returns a new random DEK, in plaintext and wrapped under
	// the provider's current master key, bound to ec.
	GenerateDataKey(ctx context.Context, ec EncryptionContext) (DataKey, error)
	// Unwrap returns the plaintext DEK. keyID identifies the master key that
	// wrapped it; ec must equal the context used at generation.
	Unwrap(ctx context.Context, wrapped []byte, keyID string, ec EncryptionContext) ([]byte, error)
}
