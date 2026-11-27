package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry returns a private Prometheus registry with Go runtime and process
// collectors. We avoid the global default registry so tests can build isolated
// registries and so no library can register high-cardinality metrics behind
// our back.
//
// Label rule: never use tenant-unbounded or per-entity labels (run_id,
// notification_id, recipient). tenant_id is allowed only on metrics documented
// as per-tenant and must be reviewed for cardinality.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
