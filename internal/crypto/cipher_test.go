package crypto

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memSource is an in-memory DEKSource standing in for the tenant store.
type memSource struct {
	mu    sync.Mutex
	keys  map[uuid.UUID]WrappedDEK
	calls atomic.Int64
	delay time.Duration
	err   error
}

func (m *memSource) WrappedDEK(ctx context.Context, id uuid.UUID) (WrappedDEK, error) {
	m.calls.Add(1)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return WrappedDEK{}, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return WrappedDEK{}, m.err
	}
	w, ok := m.keys[id]
	if !ok {
		return WrappedDEK{}, errors.New("tenant not found")
	}
	return w, nil
}

// shred mimics TenantStore.ShredDEK: the wrapped key is gone.
func (m *memSource) shred(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.keys[id]
	w.Wrapped = nil
	m.keys[id] = w
}

type harness struct {
	kp     *Local
	src    *memSource
	cipher *Cipher
	clock  *fakeClock
	reg    *prometheus.Registry
	tenant uuid.UUID
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newHarness(t testing.TB, opts CipherOptions) *harness {
	t.Helper()
	h := &harness{
		kp:    newTestLocal(t),
		src:   &memSource{keys: map[uuid.UUID]WrappedDEK{}},
		clock: &fakeClock{t: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)},
		reg:   prometheus.NewRegistry(),
	}
	opts.Now = h.clock.now
	opts.Metrics = h.reg
	h.cipher = NewCipher(h.kp, h.src, opts)
	h.tenant = h.addTenant(t)
	return h
}

// addTenant provisions a tenant DEK the way tenant creation will (slice 3).
func (h *harness) addTenant(t testing.TB) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	dk, err := h.kp.GenerateDataKey(context.Background(), TenantContext(id.String()))
	require.NoError(t, err)
	dk.Zero()
	h.src.mu.Lock()
	h.src.keys[id] = WrappedDEK{Wrapped: dk.Wrapped, KeyID: dk.KeyID, Version: 1}
	h.src.mu.Unlock()
	return id
}

func cell(tenant uuid.UUID, column string) AAD {
	return AAD{TenantID: tenant, RowID: uuid.Must(uuid.NewV7()), Column: column}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	tests := map[string][]byte{
		"empty":   {},
		"json":    []byte(`{"order_id":"A123","email":"alice@example.com"}`),
		"binary":  {0, 1, 2, 255},
		"1 MiB":   bytes.Repeat([]byte("x"), 1<<20),
		"unicode": []byte("नमस्ते 👋"),
	}
	for name, pt := range tests {
		t.Run(name, func(t *testing.T) {
			aad := cell(h.tenant, "payload")
			ct, err := h.cipher.Encrypt(ctx, aad, pt)
			require.NoError(t, err)
			assert.Len(t, ct, MinCiphertextSize+len(pt))
			if len(pt) > 8 {
				assert.False(t, bytes.Contains(ct, pt[:8]), "ciphertext leaks plaintext")
			}
			got, err := h.cipher.Decrypt(ctx, aad, ct)
			require.NoError(t, err)
			assert.Equal(t, pt, got)
		})
	}
}

func TestEncryptIsRandomised(t *testing.T) {
	h := newHarness(t, CipherOptions{})
	aad := cell(h.tenant, "payload")
	a, err := h.cipher.Encrypt(context.Background(), aad, []byte("same"))
	require.NoError(t, err)
	b, err := h.cipher.Encrypt(context.Background(), aad, []byte("same"))
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "same plaintext in the same cell must not produce the same ciphertext")
}

func TestDecryptRejectsMovedOrTamperedCiphertext(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	tenantB := h.addTenant(t)
	aad := cell(h.tenant, "payload")
	ct, err := h.cipher.Encrypt(ctx, aad, []byte(`{"secret":"value"}`))
	require.NoError(t, err)

	flip := func(i int) []byte { b := bytes.Clone(ct); b[i] ^= 0x01; return b }

	tests := []struct {
		name   string
		aad    AAD
		ct     []byte
		target error
	}{
		{name: "other row", aad: AAD{TenantID: aad.TenantID, RowID: uuid.Must(uuid.NewV7()), Column: aad.Column}, ct: ct, target: ErrDecrypt},
		{name: "other column same row", aad: AAD{TenantID: aad.TenantID, RowID: aad.RowID, Column: "recipient"}, ct: ct, target: ErrDecrypt},
		{name: "other tenant same row/column", aad: AAD{TenantID: tenantB, RowID: aad.RowID, Column: aad.Column}, ct: ct, target: ErrDecrypt},
		{name: "nonce flipped", aad: aad, ct: flip(headerSize), target: ErrDecrypt},
		{name: "body flipped", aad: aad, ct: flip(headerSize + nonceSize), target: ErrDecrypt},
		{name: "tag flipped", aad: aad, ct: flip(len(ct) - 1), target: ErrDecrypt},
		{name: "format byte changed", aad: aad, ct: flip(0), target: ErrFormat},
		{name: "dek version changed", aad: aad, ct: flip(1), target: ErrFormat},
		{name: "truncated below minimum", aad: aad, ct: ct[:MinCiphertextSize-1], target: ErrFormat},
		{name: "truncated tag", aad: aad, ct: ct[:len(ct)-1], target: ErrDecrypt},
		{name: "empty", aad: aad, ct: nil, target: ErrFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pt, err := h.cipher.Decrypt(ctx, tt.aad, tt.ct)
			require.ErrorIs(t, err, tt.target)
			assert.Nil(t, pt)
		})
	}
}

func TestInvalidAAD(t *testing.T) {
	h := newHarness(t, CipherOptions{})
	row := uuid.Must(uuid.NewV7())
	for name, aad := range map[string]AAD{
		"no tenant": {RowID: row, Column: "payload"},
		"no row":    {TenantID: h.tenant, Column: "payload"},
		"no column": {TenantID: h.tenant, RowID: row},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.cipher.Encrypt(context.Background(), aad, []byte("x"))
			require.Error(t, err)
			_, err = h.cipher.Decrypt(context.Background(), aad, make([]byte, MinCiphertextSize))
			require.Error(t, err)
		})
	}
	assert.Zero(t, h.src.calls.Load(), "invalid input must not touch the key store")
}

func TestCryptoShred(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	other := h.addTenant(t)
	aad := cell(h.tenant, "payload")
	ct, err := h.cipher.Encrypt(ctx, aad, []byte("pii"))
	require.NoError(t, err)
	otherAAD := cell(other, "payload")
	otherCT, err := h.cipher.Encrypt(ctx, otherAAD, []byte("other pii"))
	require.NoError(t, err)

	h.src.shred(h.tenant)
	h.cipher.Forget(h.tenant)

	_, err = h.cipher.Decrypt(ctx, aad, ct)
	require.ErrorIs(t, err, ErrDataKeyDestroyed)
	_, err = h.cipher.Encrypt(ctx, aad, []byte("new"))
	require.ErrorIs(t, err, ErrDataKeyDestroyed, "a shredded tenant can't encrypt new data either")

	got, err := h.cipher.Decrypt(ctx, otherAAD, otherCT)
	require.NoError(t, err, "shredding one tenant must not affect another")
	assert.Equal(t, []byte("other pii"), got)
}

func TestShredOnAnotherInstanceTakesEffectAfterTTL(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{CacheTTL: time.Minute})
	aad := cell(h.tenant, "payload")
	ct, err := h.cipher.Encrypt(ctx, aad, []byte("pii"))
	require.NoError(t, err)

	h.src.shred(h.tenant) // shredded elsewhere; this instance did not Forget
	_, err = h.cipher.Decrypt(ctx, aad, ct)
	require.NoError(t, err, "still cached within TTL (documented exposure window)")

	h.clock.advance(time.Minute)
	_, err = h.cipher.Decrypt(ctx, aad, ct)
	require.ErrorIs(t, err, ErrDataKeyDestroyed)
}

func TestKeyProviderMismatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	// Same store, different master key (e.g. wrong env key): fails closed.
	c := NewCipher(newTestLocal(t), h.src, CipherOptions{})
	_, err := c.Encrypt(ctx, cell(h.tenant, "payload"), []byte("x"))
	require.ErrorIs(t, err, ErrUnknownKey)
}

func TestWrappedDEKBoundToTenant(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	victim := h.addTenant(t)
	// A bug or attacker copies the victim's wrapped DEK onto another tenant row.
	h.src.mu.Lock()
	h.src.keys[h.tenant] = h.src.keys[victim]
	h.src.mu.Unlock()
	h.cipher.Forget(h.tenant)
	_, err := h.cipher.Encrypt(ctx, cell(h.tenant, "payload"), []byte("x"))
	require.ErrorIs(t, err, ErrDecrypt, "the encryption context binds a wrapped DEK to its tenant")
}

func TestSourceErrorsPropagate(t *testing.T) {
	h := newHarness(t, CipherOptions{})
	h.src.err = errors.New("db down")
	_, err := h.cipher.Encrypt(context.Background(), cell(h.tenant, "payload"), []byte("x"))
	require.ErrorContains(t, err, "db down")
	assert.InDelta(t, 1, testutil.ToFloat64(h.cipher.unwrapErrors), 0)
}

func TestCacheAvoidsRepeatedUnwrap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, CipherOptions{CacheTTL: time.Minute})
	for range 50 {
		_, err := h.cipher.Encrypt(ctx, cell(h.tenant, "payload"), []byte("x"))
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), h.src.calls.Load())
	assert.InDelta(t, 49, testutil.ToFloat64(h.cipher.cacheHits), 0)

	h.clock.advance(time.Minute)
	_, err := h.cipher.Encrypt(ctx, cell(h.tenant, "payload"), []byte("x"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), h.src.calls.Load(), "expired entry is reloaded")
}

func TestConcurrentMissesUnwrapOnce(t *testing.T) {
	h := newHarness(t, CipherOptions{})
	h.src.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Go(func() {
			aad := cell(h.tenant, "payload")
			ct, err := h.cipher.Encrypt(context.Background(), aad, []byte("x"))
			if err == nil {
				_, err = h.cipher.Decrypt(context.Background(), aad, ct)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), h.src.calls.Load(), "singleflight collapses concurrent misses")
}

func TestCancelledCallerDoesNotFailOthers(t *testing.T) {
	h := newHarness(t, CipherOptions{})
	h.src.delay = 100 * time.Millisecond

	cctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := h.cipher.Encrypt(cctx, cell(h.tenant, "payload"), []byte("x"))
		first <- err
	}()
	time.Sleep(10 * time.Millisecond)
	second := make(chan error, 1)
	go func() {
		_, err := h.cipher.Encrypt(context.Background(), cell(h.tenant, "payload"), []byte("x"))
		second <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	require.ErrorIs(t, <-first, context.Canceled)
	require.NoError(t, <-second, "the shared load must survive the first caller's cancellation")
}

func TestConcurrentUseUnderEvictionPressure(t *testing.T) {
	// Cache of 2 with 8 tenants forces constant eviction (and zeroing) while
	// other goroutines use keys. Any key corruption shows up as a decrypt
	// failure; any data race shows up under -race.
	ctx := context.Background()
	h := newHarness(t, CipherOptions{CacheSize: 2})
	tenants := []uuid.UUID{h.tenant}
	for range 7 {
		tenants = append(tenants, h.addTenant(t))
	}
	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := range 32 {
		wg.Go(func() {
			for j := range 50 {
				tid := tenants[(i+j)%len(tenants)]
				aad := cell(tid, "recipient")
				pt := []byte(tid.String())
				ct, err := h.cipher.Encrypt(ctx, aad, pt)
				if err != nil {
					failures.Add(1)
					continue
				}
				got, err := h.cipher.Decrypt(ctx, aad, ct)
				if err != nil || !bytes.Equal(got, pt) {
					failures.Add(1)
				}
			}
		})
	}
	wg.Wait()
	assert.Zero(t, failures.Load())
	assert.LessOrEqual(t, h.cipher.cache.len(), 2)
}

func TestCacheZeroesEvictedKeys(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	c := newDEKCache(1, time.Minute, clock.now)
	a, b := uuid.New(), uuid.New()
	keyA := bytes.Repeat([]byte{0xAA}, DEKSize)
	c.put(a, keyA, 1)

	got, _, ok := c.get(a)
	require.True(t, ok)
	got[0] = 0 // caller's copy; must not affect the cache
	again, _, _ := c.get(a)
	assert.Equal(t, byte(0xAA), again[0])

	c.put(b, bytes.Repeat([]byte{0xBB}, DEKSize), 1) // evicts a
	assert.Equal(t, make([]byte, DEKSize), keyA, "evicted key material is zeroed")
	_, _, ok = c.get(a)
	assert.False(t, ok)

	c.forget(b)
	assert.Zero(t, c.len())
}

func FuzzDecrypt(f *testing.F) {
	h := newHarness(f, CipherOptions{})
	aad := AAD{TenantID: h.tenant, RowID: uuid.Must(uuid.NewV7()), Column: "payload"}
	valid, _ := h.cipher.Encrypt(context.Background(), aad, []byte("seed"))
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte{formatV1, 1})
	f.Fuzz(func(t *testing.T, ct []byte) {
		pt, err := h.cipher.Decrypt(context.Background(), aad, ct)
		if bytes.Equal(ct, valid) {
			if err != nil || string(pt) != "seed" {
				t.Fatalf("valid ciphertext failed: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("forged ciphertext decrypted: %x", ct)
		}
	})
}

func BenchmarkEncrypt1KiB(b *testing.B) {
	h := newHarness(b, CipherOptions{})
	aad := cell(h.tenant, "payload")
	pt := bytes.Repeat([]byte("x"), 1024)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(pt)))
	for b.Loop() {
		if _, err := h.cipher.Encrypt(ctx, aad, pt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecrypt1KiB(b *testing.B) {
	h := newHarness(b, CipherOptions{})
	aad := cell(h.tenant, "payload")
	ctx := context.Background()
	ct, err := h.cipher.Encrypt(ctx, aad, bytes.Repeat([]byte("x"), 1024))
	require.NoError(b, err)
	b.ReportAllocs()
	b.SetBytes(1024)
	for b.Loop() {
		if _, err := h.cipher.Decrypt(ctx, aad, ct); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCiphertextMatchesIndependentOracle(t *testing.T) {
	// Guards against the cipher silently using the wrong key (e.g. a cached DEK
	// zeroed in place). A round trip through Cipher, even on a second
	// instance, would still pass because both sides share the bug. So decrypt
	// with a DEK unwrapped directly from the KeyProvider instead.
	ctx := context.Background()
	h := newHarness(t, CipherOptions{})
	aad := cell(h.tenant, "payload")

	w, err := h.src.WrappedDEK(ctx, h.tenant)
	require.NoError(t, err)
	dek, err := h.kp.Unwrap(ctx, w.Wrapped, w.KeyID, TenantContext(h.tenant.String()))
	require.NoError(t, err)
	require.NotEqual(t, make([]byte, DEKSize), dek)
	oracle, err := fieldAEAD(dek, aad)
	require.NoError(t, err)

	for i := range 3 { // first call loads via singleflight, later ones hit the cache
		ct, err := h.cipher.Encrypt(ctx, aad, []byte("durable"))
		require.NoError(t, err)
		got, err := oracle.Open(nil, ct[headerSize:headerSize+nonceSize], ct[headerSize+nonceSize:],
			additionalData(ct[:headerSize], aad))
		require.NoError(t, err, "call %d: ciphertext not encrypted under the tenant's real DEK", i)
		assert.Equal(t, []byte("durable"), got)
	}
}
