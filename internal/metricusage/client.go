// Package metricusage queries Coralogix Metrics Usage API (gRPC) for billing units per variation.
package metricusage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metriccommon "github.com/BigRedS/coralogix-unused-metrics-finder/internal/gen/metriccommon"
	metricusages "github.com/BigRedS/coralogix-unused-metrics-finder/internal/gen/metricusages"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/region"
	"golang.org/x/sync/errgroup"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// UnitsUsage is CX billing usage for a time-series (variation) over a date range.
type UnitsUsage struct {
	UnitUsage   float64 `json:"unit_usage"`
	BytesVolume uint64  `json:"bytes_volume"`
	Cardinality uint64  `json:"cardinality"`
	SampleCount uint64  `json:"sample_count"`
	DaysInRange int     `json:"days_in_range"`
}

// Client calls UsageService on the regional gRPC endpoint (ng-api-grpc.<domain>:443).
type Client struct {
	// GRPCHost is the host actually dialled, after mapping — worth having in diagnostics,
	// since it is deliberately not the REST API host.
	GRPCHost string
	svc      metricusages.UsageServiceClient
	conn     *grpc.ClientConn
}

// NewClient dials the Coralogix gRPC endpoint for host. Pass either the account's API host
// (api.eu2.coralogix.com) or an explicit gRPC host; region.GRPCHost normalises both, and the
// REST host is never dialled directly — see its doc comment for why that matters.
func NewClient(host, apiKey string) (*Client, error) {
	grpcHost := region.GRPCHost(host)
	target := grpcHost + ":443"
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
		grpc.WithPerRPCCredentials(bearerAuth{token: apiKey}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", target, err)
	}
	return &Client{
		GRPCHost: grpcHost,
		svc:      metricusages.NewUsageServiceClient(conn),
		conn:     conn,
	}, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

type bearerAuth struct {
	token string
}

func (b bearerAuth) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerAuth) RequireTransportSecurity() bool { return true }

// variationPageSize is the per-request cap on variations sent back by Coralogix
// (CommonRequestFields.length). Pagination is per-day: each VariationUsageDay carries
// its own OutputSetStats.matched_count, and start_offset/length slice each day independently.
// 1000 keeps responses well under the default 4MiB gRPC frame even with ~7 days × wide labels.
const variationPageSize uint32 = 1000

// FetchVariationUnits loads per-variation usage for one metric across inclusive UTC calendar dates.
// Pages through CommonRequestFields.start_offset / length until offset >= max(matched_count)
// across all days in the window.
func (c *Client) FetchVariationUnits(ctx context.Context, metricName string, startDay, endDay time.Time) (map[string]UnitsUsage, error) {
	startDay = startDay.UTC().Truncate(24 * time.Hour)
	endDay = endDay.UTC().Truncate(24 * time.Hour)
	if endDay.Before(startDay) {
		return nil, fmt.Errorf("usage end date before start date")
	}

	out := make(map[string]UnitsUsage)
	var offset uint32
	for {
		req := &metricusages.GetVariationUsagesByMetricRequest{
			Common: &metriccommon.CommonRequestFields{
				StartDate:   toProtoDate(startDay),
				EndDate:     toProtoDate(endDay),
				StartOffset: offset,
				Length:      variationPageSize,
			},
			MetricName: metricName,
		}

		resp, err := c.svc.GetVariationUsagesByMetric(ctx, req)
		if err != nil {
			return nil, err
		}

		var maxMatched uint32
		for _, day := range resp.GetDailyUsages() {
			for _, v := range day.GetVariationUsages() {
				key := VariationKey(metricName, v.GetLabelNames())
				agg := out[key]
				if u := v.GetUsage(); u != nil {
					agg.UnitUsage += float64(u.GetUnitUsage())
					agg.BytesVolume += u.GetBytesVolume()
					agg.Cardinality += u.GetCardinality()
				}
				agg.SampleCount += v.GetSampleCount()
				agg.DaysInRange++
				out[key] = agg
			}
			if mc := day.GetOutputSetStats().GetMatchedCount(); mc > maxMatched {
				maxMatched = mc
			}
		}

		offset += variationPageSize
		if offset >= maxMatched {
			return out, nil
		}
	}
}

// DebugRawVariations returns the unparsed first page of GetVariationUsagesByMetric for one
// metric across the inclusive UTC date range. Intended for debugging "unit_usage=0 everywhere"
// symptoms — callers should print the response fields directly.
func (c *Client) DebugRawVariations(ctx context.Context, metricName string, startDay, endDay time.Time) (*metricusages.GetVariationUsagesByMetricResponse, error) {
	startDay = startDay.UTC().Truncate(24 * time.Hour)
	endDay = endDay.UTC().Truncate(24 * time.Hour)
	if endDay.Before(startDay) {
		return nil, fmt.Errorf("usage end date before start date")
	}
	req := &metricusages.GetVariationUsagesByMetricRequest{
		Common: &metriccommon.CommonRequestFields{
			StartDate:   toProtoDate(startDay),
			EndDate:     toProtoDate(endDay),
			StartOffset: 0,
			Length:      variationPageSize,
		},
		MetricName: metricName,
	}
	return c.svc.GetVariationUsagesByMetric(ctx, req)
}

// VariationKey produces a stable key identifying a Coralogix billing variation. CX groups
// series into variations by the SET OF LABEL NAMES present (not by label values), so the
// key is the sorted set of label keys (always including "__name__"). Multiple catalog
// series whose label keys equal the variation's key share that variation's billed usage.
//
// CX may emit label_names either as bare names ("job") or as "name=value" pairs depending
// on the API version; either way only the key portion contributes — values are discarded.
func VariationKey(metricName string, labelNames []string) string {
	keys := map[string]struct{}{"__name__": {}}
	for _, s := range labelNames {
		k := s
		if i := strings.IndexByte(s, '='); i >= 0 {
			k = s[:i]
		}
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	return metricName + "|" + strings.Join(sorted, ",")
}

// variationKeyFromCatalogLabels builds the same VariationKey from a catalog series'
// labels map (using only the label names, ignoring values).
func variationKeyFromCatalogLabels(labels map[string]string) string {
	mn := labels["__name__"]
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return mn + "|" + strings.Join(keys, ",")
}

func splitUsage(u UnitsUsage, n int) UnitsUsage {
	if n <= 1 {
		return u
	}
	return UnitsUsage{
		UnitUsage:   u.UnitUsage / float64(n),
		BytesVolume: u.BytesVolume / uint64(n),
		Cardinality: u.Cardinality / uint64(n),
		SampleCount: u.SampleCount / uint64(n),
		DaysInRange: u.DaysInRange,
	}
}

// MetricFetchError is a billing lookup that failed for one metric name.
type MetricFetchError struct {
	MetricName string
	Err        error
}

func (e MetricFetchError) Error() string {
	return fmt.Sprintf("%q: %v", e.MetricName, e.Err)
}

func (e MetricFetchError) Unwrap() error { return e.Err }

// EnrichCatalog maps variation-level CX billing onto Prometheus catalog series keys.
// A CX "variation" is identified by its label-name set: every catalog series whose labels
// have the same key set as the variation belongs to that variation, and the variation's
// billed usage is divided evenly across them.
//
// Per-metric fetch errors are returned as MetricFetchError values rather than aborting the whole
// batch — a single 5xx from CX must not throw away every other metric's billing data. They are
// returned unformatted so callers can group them by cause instead of emitting one message per
// metric. The error return is reserved for the caller's ctx being cancelled.
func (c *Client) EnrichCatalog(
	ctx context.Context,
	catalogSeries map[string]map[string]string,
	metricNames []string,
	startDay, endDay time.Time,
	workers int,
	onProgress func(done, total int, metric string),
) (map[string]UnitsUsage, map[string]int, []MetricFetchError, error) {
	if workers <= 0 {
		workers = 4
	}

	seriesKeysByMetric := make(map[string][]string)
	for sk, lbls := range catalogSeries {
		mn := lbls["__name__"]
		if mn == "" {
			continue
		}
		seriesKeysByMetric[mn] = append(seriesKeysByMetric[mn], sk)
	}

	result := make(map[string]UnitsUsage)
	splitCount := make(map[string]int)
	var failures []MetricFetchError
	var mu sync.Mutex

	total := len(metricNames)
	var done atomic.Int32

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, workers)

	for _, mname := range metricNames {
		mname := mname
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return gctx.Err()
			}
			defer func() { <-sem }()

			skList := seriesKeysByMetric[mname]
			if len(skList) == 0 {
				if onProgress != nil {
					onProgress(int(done.Add(1)), total, mname)
				}
				return nil
			}

			byVar, err := c.FetchVariationUnits(gctx, mname, startDay, endDay)
			if err != nil {
				// If the parent ctx is done, bubble the cancellation up so the whole batch stops.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				mu.Lock()
				failures = append(failures, MetricFetchError{MetricName: mname, Err: err})
				mu.Unlock()
				if onProgress != nil {
					onProgress(int(done.Add(1)), total, mname)
				}
				return nil
			}

			// Group catalog series by their label-name set; that's the unit CX bills against.
			byVarKey := make(map[string][]string)
			for _, sk := range skList {
				vk := variationKeyFromCatalogLabels(catalogSeries[sk])
				byVarKey[vk] = append(byVarKey[vk], sk)
			}

			localResult := make(map[string]UnitsUsage)
			localSplit := make(map[string]int)
			for vk, usage := range byVar {
				group := byVarKey[vk]
				if len(group) == 0 {
					continue
				}
				part := splitUsage(usage, len(group))
				for _, sk := range group {
					localResult[sk] = part
					localSplit[sk] = len(group)
				}
			}

			mu.Lock()
			for k, v := range localResult {
				result[k] = v
			}
			for k, v := range localSplit {
				splitCount[k] = v
			}
			mu.Unlock()

			if onProgress != nil {
				onProgress(int(done.Add(1)), total, mname)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, err
	}
	// Failures arrive from parallel workers; sort so warnings are stable across runs.
	sort.Slice(failures, func(i, j int) bool { return failures[i].MetricName < failures[j].MetricName })
	return result, splitCount, failures, nil
}

func toProtoDate(t time.Time) *date.Date {
	t = t.UTC()
	return &date.Date{
		Year:  int32(t.Year()),
		Month: int32(t.Month()),
		Day:   int32(t.Day()),
	}
}
