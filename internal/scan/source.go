package scan

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
)

// Source supplies the Coralogix metadata and metric-catalog data the scan needs.
//
// It is the seam between the scan engine and the transport used to reach
// Coralogix. The API-key-backed *coralogix.Client implements it, and so does the
// OAuth/cx-CLI-backed *cxcli.Client — letting the same scan run over either.
//
// FetchSeriesForMetric returns (series, truncated, error). A returned
// *coralogix.SeriesAnalysisLimitError marks a metric as unanalyzable: the scan
// records it as a fetch failure and continues rather than aborting.
type Source interface {
	FetchDashboardCatalog(ctx context.Context) ([]coralogix.DashboardCatalogItem, error)
	FetchDashboard(ctx context.Context, dashboardID string) (json.RawMessage, error)
	FetchAllAlertDefs(ctx context.Context, pageSize int) ([]json.RawMessage, error)
	FetchSLOs(ctx context.Context) ([]json.RawMessage, error)
	FetchMetricNames(ctx context.Context, start, end time.Time) ([]string, error)
	FetchSeriesForMetric(ctx context.Context, metricName string, start, end time.Time, limit int) ([]map[string]string, bool, error)
	// Host returns a human-readable identifier for the target (API host or
	// cx profile) recorded in report metadata.
	Host() string
}
