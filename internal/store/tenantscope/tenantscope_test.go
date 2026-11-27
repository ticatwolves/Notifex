package tenantscope

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestScope(t *testing.T) {
	ctx := context.Background()
	_, ok := From(ctx)
	assert.False(t, ok)

	_, ok = From(With(ctx, uuid.Nil))
	assert.False(t, ok, "nil uuid is not a scope")

	id := uuid.New()
	got, ok := From(With(ctx, id))
	assert.True(t, ok)
	assert.Equal(t, id, got)

	_, ok = Bypassed(ctx)
	assert.False(t, ok)
	r, ok := Bypassed(Bypass(ctx, ReasonAPIKeyLookup))
	assert.True(t, ok)
	assert.Equal(t, ReasonAPIKeyLookup, r)
	_, ok = Bypassed(Bypass(ctx, ""))
	assert.False(t, ok, "empty reason is not a bypass")
}
