package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sync/singleflight"
)

// Field ciphertext format v1:
//
//	format(1)=0x01 | dek_version(1) | nonce(12) | AES-256-GCM ciphertext+tag(16)
//
// The key is HKDF-SHA256(DEK, info = "notifex/field/v1" | tenant | row | column),
// so each derived key encrypts only the few values written to one cell. That
// keeps random 96-bit nonces far below the NIST 2^32-messages-per-key limit,
// which a single per-tenant key would hit within months at high volume. The
// AEAD additional data repeats the header and the cell coordinates, so header
// tampering and cross-cell moves both fail authentication.
const formatV1 byte = 0x01

const (
	nonceSize  = 12
	headerSize = 2
	tagSize    = 16
	// MinCiphertextSize is the size of an encrypted empty value.
	MinCiphertextSize = headerSize + nonceSize + tagSize
)

var hkdfInfoPrefix = []byte("notifex/field/v1")

// AAD names the cell a value lives in. All three parts are bound to the
// ciphertext.
type AAD struct {
	TenantID uuid.UUID
	// RowID is the id of the row holding the value (notification id, run id,
	// credential id, …).
	RowID uuid.UUID
	// Column is the column name, so payload and recipient ciphertexts in the
	// same row can't be swapped.
	Column string
}

func (a AAD) validate() error {
	if a.TenantID == uuid.Nil || a.RowID == uuid.Nil || a.Column == "" {
		return errors.New("crypto: AAD requires tenant id, row id and column")
	}
	return nil
}

func (a AAD) cell() []byte {
	b := make([]byte, 0, 32+4+len(a.Column))
	b = append(b, a.TenantID[:]...)
	b = append(b, a.RowID[:]...)
	return appendLP(b, a.Column)
}

// WrappedDEK is a tenant's DEK as persisted on the tenant row.
type WrappedDEK struct {
	Wrapped []byte
	KeyID   string
	// Version is the tenant's DEK generation. Always 1 until DEK rotation
	// exists; it is written into every ciphertext so rotation needs no format change.
	Version uint8
}

// DEKSource loads a tenant's wrapped DEK. The store implements it. It returns
// ErrDataKeyDestroyed for a crypto-shredded tenant.
type DEKSource interface {
	WrappedDEK(ctx context.Context, tenantID uuid.UUID) (WrappedDEK, error)
}

// FieldCipher encrypts and decrypts sensitive column values under the
// tenant's DEK.
type FieldCipher interface {
	Encrypt(ctx context.Context, aad AAD, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, aad AAD, ciphertext []byte) ([]byte, error)
	// Forget drops a tenant's cached DEK on this instance (call after shred).
	Forget(tenantID uuid.UUID)
}

// CipherOptions configures NewCipher.
type CipherOptions struct {
	// CacheSize bounds the number of unwrapped DEKs held in memory. Default 10000.
	CacheSize int
	// CacheTTL bounds how long an unwrapped DEK is reused. It is also the
	// longest a shredded tenant's key stays usable on other instances. Default 5m.
	CacheTTL time.Duration
	// Metrics, if set, receives cache and unwrap metrics.
	Metrics prometheus.Registerer
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Cipher is the FieldCipher implementation.
type Cipher struct {
	keys   KeyProvider
	source DEKSource
	cache  *dekCache
	group  singleflight.Group

	cacheHits, cacheMisses prometheus.Counter
	unwrapErrors           prometheus.Counter
}

var _ FieldCipher = (*Cipher)(nil)

// NewCipher builds a Cipher.
func NewCipher(keys KeyProvider, source DEKSource, opts CipherOptions) *Cipher {
	if opts.CacheSize <= 0 {
		opts.CacheSize = 10000
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	lookups := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notifex_crypto_dek_cache_lookups_total",
		Help: "Tenant DEK cache lookups by result.",
	}, []string{"result"})
	unwrapErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "notifex_crypto_dek_unwrap_errors_total",
		Help: "Failed tenant DEK loads or unwraps.",
	})
	if opts.Metrics != nil {
		opts.Metrics.MustRegister(lookups, unwrapErrors)
	}
	return &Cipher{
		keys:         keys,
		source:       source,
		cache:        newDEKCache(opts.CacheSize, opts.CacheTTL, opts.Now),
		cacheHits:    lookups.WithLabelValues("hit"),
		cacheMisses:  lookups.WithLabelValues("miss"),
		unwrapErrors: unwrapErrors,
	}
}

// Encrypt implements FieldCipher.
func (c *Cipher) Encrypt(ctx context.Context, aad AAD, plaintext []byte) ([]byte, error) {
	if err := aad.validate(); err != nil {
		return nil, err
	}
	dek, version, err := c.dek(ctx, aad.TenantID)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	aead, err := fieldAEAD(dek, aad)
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerSize+nonceSize, headerSize+nonceSize+len(plaintext)+tagSize)
	out[0], out[1] = formatV1, version
	if _, err := rand.Read(out[headerSize:]); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	return aead.Seal(out, out[headerSize:], plaintext, additionalData(out[:headerSize], aad)), nil
}

// Decrypt implements FieldCipher.
func (c *Cipher) Decrypt(ctx context.Context, aad AAD, ciphertext []byte) ([]byte, error) {
	if err := aad.validate(); err != nil {
		return nil, err
	}
	if len(ciphertext) < MinCiphertextSize || ciphertext[0] != formatV1 {
		return nil, ErrFormat
	}
	dek, version, err := c.dek(ctx, aad.TenantID)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	if ciphertext[1] != version {
		// Rotation is not implemented: there is exactly one DEK version per tenant.
		return nil, fmt.Errorf("%w: dek version %d, tenant has %d", ErrFormat, ciphertext[1], version)
	}
	aead, err := fieldAEAD(dek, aad)
	if err != nil {
		return nil, err
	}
	nonce := ciphertext[headerSize : headerSize+nonceSize]
	// Non-nil dst so an encrypted empty value decrypts to []byte{}, never nil:
	// callers map nil to SQL NULL (purged), which is a different state.
	body := ciphertext[headerSize+nonceSize:]
	pt, err := aead.Open(make([]byte, 0, len(body)-tagSize), nonce, body, additionalData(ciphertext[:headerSize], aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// Forget implements FieldCipher. A load already in flight may still finish and
// re-cache the key it fetched before the shred; callers shred the stored key
// first, so at most one TTL of exposure remains, as with other instances.
func (c *Cipher) Forget(tenantID uuid.UUID) {
	c.group.Forget(tenantID.String())
	c.cache.forget(tenantID)
}

// dek returns a private copy of the tenant's plaintext DEK; callers zero it.
func (c *Cipher) dek(ctx context.Context, tenant uuid.UUID) ([]byte, uint8, error) {
	if key, v, ok := c.cache.get(tenant); ok {
		c.cacheHits.Inc()
		return key, v, nil
	}
	c.cacheMisses.Inc()

	// Collapse concurrent misses for one tenant into a single unwrap (KMS calls
	// are slow, billed and rate-limited). The load ignores the caller's
	// cancellation so one cancelled request can't fail the others waiting on it.
	ch := c.group.DoChan(tenant.String(), func() (any, error) {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		key, version, err := c.load(lctx, tenant)
		if err != nil {
			return nil, err
		}
		c.cache.put(tenant, key, version) // cache owns key from here; callers read copies via get
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return nil, 0, fmt.Errorf("crypto: load dek: %w", ctx.Err())
	case res := <-ch:
		if res.Err != nil {
			return nil, 0, res.Err
		}
		// Read back through the cache so the caller gets its own copy.
		if key, v, ok := c.cache.get(tenant); ok {
			return key, v, nil
		}
		// Evicted between put and get (tiny cache under pressure): load directly.
		return c.load(ctx, tenant)
	}
}

func (c *Cipher) load(ctx context.Context, tenant uuid.UUID) ([]byte, uint8, error) {
	ctx, span := otel.Tracer("notifex/crypto").Start(ctx, "crypto.unwrap_dek")
	defer span.End()
	span.SetAttributes(attribute.String("tenant_id", tenant.String()))

	w, err := c.source.WrappedDEK(ctx, tenant)
	if err == nil && len(w.Wrapped) == 0 {
		err = ErrDataKeyDestroyed
	}
	if err != nil {
		c.unwrapErrors.Inc()
		span.SetStatus(codes.Error, "load wrapped dek")
		return nil, 0, fmt.Errorf("crypto: load wrapped dek: %w", err)
	}
	key, err := c.keys.Unwrap(ctx, w.Wrapped, w.KeyID, TenantContext(tenant.String()))
	if err != nil {
		c.unwrapErrors.Inc()
		span.SetStatus(codes.Error, "unwrap dek")
		return nil, 0, fmt.Errorf("crypto: unwrap dek: %w", err)
	}
	if len(key) != DEKSize {
		clear(key)
		c.unwrapErrors.Inc()
		return nil, 0, fmt.Errorf("crypto: unwrapped dek has %d bytes", len(key))
	}
	return key, w.Version, nil
}

func fieldAEAD(dek []byte, aad AAD) (cipher.AEAD, error) {
	info := append(append([]byte{}, hkdfInfoPrefix...), aad.cell()...)
	sub, err := hkdf.Key(sha256.New, dek, nil, string(info), DEKSize)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive field key: %w", err)
	}
	defer clear(sub)
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, fmt.Errorf("crypto: field cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: field cipher: %w", err)
	}
	return aead, nil
}

func additionalData(header []byte, aad AAD) []byte {
	return append(append([]byte{}, header...), aad.cell()...)
}
