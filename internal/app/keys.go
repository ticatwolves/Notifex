package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/ticatwolves/notifex/internal/crypto"
)

// KeyProvider wraps tenant DEKs: the local master key in local runs; KMS
// (Phase 8) everywhere else. Until KMS exists, deployed environments get a
// provider that refuses every key operation, so tenant creation (and any
// decrypt) fails there while the rest of the service keeps working. Never
// fall back to the local key.
func KeyProvider(rt *Runtime) (crypto.KeyProvider, error) {
	if rt.Config.Deployed() {
		rt.Logger.Warn("no KMS KeyProvider yet (Phase 8): tenant keys are unavailable in this environment")
		return noKMS{}, nil
	}
	kp, err := crypto.NewLocalFromBase64(rt.Config.LocalMasterKey)
	if err != nil {
		return nil, fmt.Errorf("NOTIFEX_LOCAL_MASTER_KEY: %w", err)
	}
	return kp, nil
}

// noKMS refuses every key operation (deployed environments before Phase 8).
type noKMS struct{}

var errNoKMS = errors.New("tenant encryption keys are unavailable: KMS KeyProvider not configured (Phase 8)")

func (noKMS) GenerateDataKey(context.Context, crypto.EncryptionContext) (crypto.DataKey, error) {
	return crypto.DataKey{}, errNoKMS
}

func (noKMS) Unwrap(context.Context, []byte, string, crypto.EncryptionContext) ([]byte, error) {
	return nil, errNoKMS
}
