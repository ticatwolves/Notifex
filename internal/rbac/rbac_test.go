package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ticatwolves/notifex/internal/store"
)

// The expected matrix is written out literally (not derived from the code),
// so any change to who may do what shows up as a diff in this table.
// Columns: viewer, developer, admin, owner.
var matrix = map[Capability][4]bool{
	ViewTenant:           {true, true, true, true},
	EditWorkflows:        {false, true, true, true},
	ManageApplications:   {false, true, true, true},
	ManageAPIKeysNonProd: {false, true, true, true},
	ViewCredentialMeta:   {false, true, true, true},
	OperateNotifications: {false, true, true, true},
	ManageAPIKeysProd:    {false, false, true, true},
	DeleteApplications:   {false, false, true, true},
	UpdateTenant:         {false, false, true, true},
	ManageMembers:        {false, false, true, true},
	ManageSettings:       {false, false, true, true},
	ViewAudit:            {false, false, true, true},
	DeleteTenant:         {false, false, false, true},
	ManageBilling:        {false, false, false, true},
	TransferOwnership:    {false, false, false, true},
}

var roles = [4]store.Role{store.RoleViewer, store.RoleDeveloper, store.RoleAdmin, store.RoleOwner}

func TestEveryRoleTimesEveryCapability(t *testing.T) {
	for c, want := range matrix {
		for i, r := range roles {
			assert.Equal(t, want[i], Allow(r, c), "%s × %s", r, c)
		}
	}
}

func TestMatrixCoversEveryCapability(t *testing.T) {
	for _, c := range All() {
		_, ok := matrix[c]
		assert.True(t, ok, "capability %s missing from the matrix test", c)
	}
	assert.Len(t, matrix, len(All()), "matrix has capabilities the code doesn't define")
}

func TestUnknownRoleHasNothing(t *testing.T) {
	for _, c := range All() {
		assert.False(t, Allow("superuser", c))
		assert.False(t, Allow("", c))
	}
	assert.False(t, Valid("superuser"))
	assert.Equal(t, 0, Rank("superuser"))
}

func TestRankOrder(t *testing.T) {
	for i := 1; i < len(roles); i++ {
		assert.Less(t, Rank(roles[i-1]), Rank(roles[i]))
	}
}
