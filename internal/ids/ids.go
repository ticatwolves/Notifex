// Package ids formats public resource identifiers: <kind>_<22 base62 chars>.
//
// Internally every id is a UUIDv7. At the API boundary it is prefixed with its
// kind so ids are self-describing (a run id can never be mistaken for an app
// id) and opaque. base62 keeps them URL-safe and double-click selectable.
// 22 base62 digits hold 131 bits, enough for any 128-bit UUID.
package ids

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
)

// Kind is an id prefix.
type Kind string

// Public id kinds.
const (
	Tenant       Kind = "ten"
	Application  Kind = "app"
	Environment  Kind = "env"
	APIKey       Kind = "key"
	Run          Kind = "run"
	Notification Kind = "ntf"
	User         Kind = "usr"
	Membership   Kind = "mem"
	Invitation   Kind = "inv"
	Session      Kind = "ses"
	AuditEvent   Kind = "evt"
)

const encodedLen = 22

// ErrInvalid means the string is not a valid id of the expected kind.
var ErrInvalid = errors.New("ids: invalid id")

var maxUUID = new(big.Int).Lsh(big.NewInt(1), 128)

// Format renders id as <kind>_<base62>. The alphabet is 0-9A-Za-z, which is
// ASCII order, so encoded ids sort like the UUIDs (UUIDv7: creation order).
func Format(k Kind, id uuid.UUID) string {
	s := swapCase(new(big.Int).SetBytes(id[:]).Text(62))
	return string(k) + "_" + strings.Repeat("0", encodedLen-len(s)) + s
}

// Parse decodes s, which must be an id of kind k.
func Parse(k Kind, s string) (uuid.UUID, error) {
	enc, ok := strings.CutPrefix(s, string(k)+"_")
	if !ok || len(enc) != encodedLen {
		return uuid.Nil, fmt.Errorf("%w: want %s_<%d chars>", ErrInvalid, k, encodedLen)
	}
	n, ok := new(big.Int).SetString(swapCase(enc), 62)
	if !ok || n.Sign() < 0 || n.Cmp(maxUUID) >= 0 {
		return uuid.Nil, ErrInvalid
	}
	var id uuid.UUID
	n.FillBytes(id[:])
	if id == uuid.Nil {
		// Never a real id; letting it through would reach the store as an
		// unscoped tenant and fail as a 500 instead of a 404.
		return uuid.Nil, ErrInvalid
	}
	return id, nil
}

// swapCase maps between big.Int's base-62 alphabet (0-9a-zA-Z) and the
// ASCII-ordered alphabet we expose (0-9A-Za-z): digit values 10-35 and 36-61
// trade letter cases.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}
