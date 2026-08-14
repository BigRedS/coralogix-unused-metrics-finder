package strintern

import (
	"encoding/json"
	"runtime"
	"strconv"
	"testing"
)

// seriesProfile describes a synthetic series shape: labels whose values repeat across the whole
// account, and labels whose values are unique per series.
type seriesProfile struct {
	name     string
	repeated map[string]string
	unique   []string // label names given a per-series value
}

// narrow is a plain k8s pod metric. wide is closer to a real Coralogix account, where OTel
// resource attributes and Coralogix's own application/subsystem labels widen every series.
var (
	narrowProfile = seriesProfile{
		name: "8 labels, 2 unique",
		repeated: map[string]string{
			"__name__":     "process_memory_usage_By",
			"job":          "kubernetes-pods",
			"namespace":    "production",
			"node":         "ip-10-0-1-12.eu-west-1.compute.internal",
			"container":    "checkout",
			"service_name": "checkout-service",
		},
		unique: []string{"pod", "instance"},
	}
	wideProfile = seriesProfile{
		name: "18 labels, 3 unique",
		repeated: map[string]string{
			"__name__":               "process_memory_usage_By",
			"job":                    "kubernetes-pods",
			"namespace":              "production",
			"node":                   "ip-10-0-1-12.eu-west-1.compute.internal",
			"container":              "checkout",
			"service_name":           "checkout-service",
			"service_version":        "2026.8.1-a3f9c2e",
			"cx_application_name":    "shop",
			"cx_subsystem_name":      "checkout",
			"deployment_environment": "production",
			"k8s_cluster_name":       "eu-west-1-prod-blue",
			"cloud_region":           "eu-west-1",
			"cloud_provider":         "aws",
			"telemetry_sdk_name":     "opentelemetry",
			"telemetry_sdk_language": "java",
		},
		unique: []string{"pod", "instance", "k8s_pod_uid"},
	}
)

// buildCatalog mimics the scan's retained catalog: one label map per series, decoded from JSON
// exactly as FetchSeriesForMetric does. Decoding matters — encoding/json allocates a fresh
// string for every key and value it reads, which is the duplication interning removes. Building
// the maps from Go literals instead would silently share the compiler's constant strings and
// measure nothing.
func buildCatalog(p seriesProfile, n int, tbl *Table) map[string]map[string]string {
	catalog := make(map[string]map[string]string, n)
	for i := 0; i < n; i++ {
		id := strconv.Itoa(i)

		payload := "{"
		for k, v := range p.repeated {
			payload += `"` + k + `":"` + v + `",`
		}
		for j, k := range p.unique {
			payload += `"` + k + `":"checkout-7d9f8b6c4d-` + id + "-" + strconv.Itoa(j) + `"`
			if j < len(p.unique)-1 {
				payload += ","
			}
		}
		payload += "}"

		var lbls map[string]string
		if err := json.Unmarshal([]byte(payload), &lbls); err != nil {
			panic(err)
		}
		if tbl != nil {
			lbls = tbl.Labels(lbls)
		}
		catalog[`process_memory_usage_By{pod="checkout-service-7d9f8b6c4d-`+id+`"}`] = lbls
	}
	return catalog
}

// retainedBytes measures the heap held by the catalog after build returns. build must drop any
// intern table before returning — the scan does exactly that once the catalog is complete, so
// the table's own entries must not be counted against the steady-state footprint.
func retainedBytes(build func() map[string]map[string]string) (perSeries float64, total uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	catalog := build()

	runtime.GC()
	runtime.ReadMemStats(&after)
	total = after.HeapAlloc - before.HeapAlloc
	perSeries = float64(total) / float64(len(catalog))

	runtime.KeepAlive(catalog)
	return perSeries, total
}

// Interning must materially cut what the catalog retains — that reduction is why the scan
// interns at all, so a regression here is a regression in the largest accounts.
func TestInterningReducesCatalogRetention(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation distorts allocation; run without -race to measure retention")
	}
	const series = 200_000

	for _, p := range []seriesProfile{narrowProfile, wideProfile} {
		t.Run(p.name, func(t *testing.T) {
			rawPer, rawTotal := retainedBytes(func() map[string]map[string]string {
				return buildCatalog(p, series, nil)
			})

			distinct := 0
			internedPer, internedTotal := retainedBytes(func() map[string]map[string]string {
				tbl := New()
				c := buildCatalog(p, series, tbl)
				distinct = tbl.Len()
				return c // tbl goes out of scope here, as it does in scan.Run
			})

			saved := 1 - internedPer/rawPer
			t.Logf("%d series, %s: raw %.0f B/series (%.0f MiB), interned %.0f B/series (%.0f MiB) — %.0f%% smaller, %d distinct strings",
				series, p.name,
				rawPer, float64(rawTotal)/(1<<20),
				internedPer, float64(internedTotal)/(1<<20),
				saved*100, distinct)

			if saved < 0.20 {
				t.Errorf("interning saved only %.0f%% of catalog memory (%.0f -> %.0f B/series); want at least 20%%",
					saved*100, rawPer, internedPer)
			}
		})
	}
}
