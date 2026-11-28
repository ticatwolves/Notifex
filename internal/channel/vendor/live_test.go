package vendor_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
	"github.com/ticatwolves/notifex/internal/credential"
)

// TestEveryProviderHasASender: a provider tenants can configure but the
// dispatcher can't send through would fail every notification.
func TestEveryProviderHasASender(t *testing.T) {
	t.Parallel()
	live, mock := channel.NewRegistry(), channel.NewRegistry()
	vendor.RegisterLive(live, vendor.LiveConfig{AWS: aws.Config{Region: "ap-south-1"}, PlatformFrom: "n@mail.notifex.test", EHLOName: "notifex"})
	vendor.NewMock().RegisterAll(mock)
	for _, p := range credential.Catalog {
		_, err := live.Lookup(p.Channel, p.Name)
		assert.NoError(t, err, "live %s/%s", p.Channel, p.Name)
		_, err = mock.Lookup(p.Channel, p.Name)
		assert.NoError(t, err, "mock %s/%s", p.Channel, p.Name)
	}
}
