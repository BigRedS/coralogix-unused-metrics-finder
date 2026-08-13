// Command webui is a small localhost server to run coralogix-unused-metrics-finder scans from a browser.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/cxteams"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/metricusage"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/region"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/scan"
)

const otelProcessorsBase = "metric_usage_otel_processors.yaml"

//go:embed index.html
var indexHTML []byte

func main() {
	listen := flag.String("listen", "localhost:8765", "HTTP listen address")
	jobTTL := flag.Duration("job-ttl", time.Hour, "how long to keep job dirs and download links")
	flag.Parse()

	reg := newRegistry(*jobTTL)
	mux := http.NewServeMux()

	mux.Handle("GET /{$}", serveIndex())
	mux.Handle("GET /api/regions", http.HandlerFunc(reg.handleRegions))
	mux.Handle("POST /api/run", http.HandlerFunc(reg.handleRun))
	mux.Handle("GET /api/job/{id}", http.HandlerFunc(reg.handleJobStatus))
	mux.Handle("GET /api/job/{id}/otel-yaml", http.HandlerFunc(reg.handleOTELYAMLPreview))
	mux.Handle("GET /dl/{id}/{file}", http.HandlerFunc(reg.handleDownload))

	log.Printf("coralogix-unused-metrics-finder webui listening on http://%s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func serveIndex() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
}

type registry struct {
	mu   sync.Mutex
	jobs map[string]*job
	ttl  time.Duration
}

func newRegistry(ttl time.Duration) *registry {
	r := &registry{jobs: make(map[string]*job), ttl: ttl}
	go r.pruneLoop()
	return r
}

func (reg *registry) pruneLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		reg.prune()
	}
}

func (reg *registry) prune() {
	cutoff := time.Now().Add(-reg.ttl)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for id, j := range reg.jobs {
		j.mu.Lock()
		created := j.created
		dir := j.dir
		j.mu.Unlock()
		if created.Before(cutoff) {
			if dir != "" {
				_ = os.RemoveAll(dir)
			}
			delete(reg.jobs, id)
		}
	}
}

func (reg *registry) handleRegions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"regions": region.SortedRegionChoices(),
	})
}

type runRequest struct {
	Region string `json:"region"`
	APIKey string `json:"api_key"`
	// Billing mirrors the CLI's --billing: off by default, because the cost lookup is slow and
	// its per-series outputs are by far the largest files in the download.
	Billing bool `json:"billing"`
}

func (reg *registry) handleRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 512*1024))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req runRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Region = strings.TrimSpace(req.Region)
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "api_key required"})
		return
	}
	apiHost, err := region.ResolveAPIHost(req.Region)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	id, err := randomID()
	if err != nil {
		http.Error(w, "id", http.StatusInternalServerError)
		return
	}

	j := &job{id: id, status: "queued", created: time.Now()}
	reg.mu.Lock()
	reg.jobs[id] = j
	reg.mu.Unlock()

	go reg.runScan(j, apiHost, req.APIKey, req.Billing)

	writeJSON(w, http.StatusOK, map[string]string{"job_id": id})
}

func (reg *registry) runScan(j *job, apiHost, apiKey string, billing bool) {
	j.setStatus("running")

	dir, err := os.MkdirTemp("", "coralogix-unused-metrics-finder-webui-*")
	if err != nil {
		j.fail(fmt.Errorf("temp dir: %w", err))
		return
	}
	j.setDir(dir)

	ctx := context.Background()
	client := coralogix.NewClient(apiHost, apiKey, 120*time.Second)

	var billingClient *metricusage.Client
	usageLookbackDays := 0
	if billing {
		billingClient, err = metricusage.NewClient(region.GRPCHost(apiHost), apiKey)
		if err != nil {
			j.fail(fmt.Errorf("billing client: %w", err))
			_ = os.RemoveAll(dir)
			return
		}
		defer billingClient.Close()
		usageLookbackDays = 7
	}

	rep, err := scan.Run(ctx, client, scan.Options{
		SeriesLookback:             25 * time.Hour,
		SeriesLimitPerMetric:       50_000,
		Workers:                    8,
		UsageLookbackDays:          usageLookbackDays,
		UsageBillingCalendarMonths: 0,
		Billing:                    billingClient,
		Quiet:                      true,
		SkipDashboards:             false,
		SkipAlerts:                 false,
		SkipSLOs:                   false,
	})
	if err != nil {
		j.fail(fmt.Errorf("scan: %w", err))
		_ = os.RemoveAll(dir)
		return
	}
	teamPrefix := resolveTeamFilenamePrefix(ctx, region.GRPCHost(apiHost), apiKey)
	written, err := rep.Write(dir, teamPrefix)
	if err != nil {
		j.fail(fmt.Errorf("write: %w", err))
		_ = os.RemoveAll(dir)
		return
	}
	j.setFiles(written)
	j.setStatus("done")
}

// resolveTeamFilenamePrefix mirrors the CLI behavior: try ListTeams, return a sanitized
// prefix, fall back to empty string on any failure so the scan still produces output.
func resolveTeamFilenamePrefix(ctx context.Context, grpcHost, apiKey string) string {
	tc, err := cxteams.NewClient(grpcHost, apiKey)
	if err != nil {
		return ""
	}
	defer tc.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, err := tc.FetchTeamName(cctx)
	if err != nil || name == "" {
		return ""
	}
	return cxteams.SanitizeForFilename(name)
}

func (reg *registry) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reg.mu.Lock()
	j, ok := reg.jobs[id]
	reg.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown job"})
		return
	}

	st, errMsg := j.snapshot()
	out := map[string]any{
		"status": st,
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	if st == "done" {
		out["files"] = j.getFiles()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (reg *registry) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base := r.PathValue("file")
	if base != filepath.Base(base) || strings.Contains(base, "..") {
		http.NotFound(w, r)
		return
	}
	reg.mu.Lock()
	j, ok := reg.jobs[id]
	reg.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	st, _ := j.snapshot()
	if st != "done" {
		http.Error(w, "job not finished", http.StatusConflict)
		return
	}

	allowed := false
	for _, f := range j.getFiles() {
		if f == base {
			allowed = true
			break
		}
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}

	dir := j.getDir()
	if dir == "" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, base)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Disposition", `attachment; filename="`+base+`"`)
	http.ServeFile(w, r, path)
}

func (reg *registry) handleOTELYAMLPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reg.mu.Lock()
	j, ok := reg.jobs[id]
	reg.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	st, _ := j.snapshot()
	if st != "done" {
		http.Error(w, "job not finished", http.StatusConflict)
		return
	}
	dir := j.getDir()
	if dir == "" {
		http.NotFound(w, r)
		return
	}
	otelName := otelProcessorsBase
	for _, f := range j.getFiles() {
		if strings.HasSuffix(f, otelProcessorsBase) {
			otelName = f
			break
		}
	}
	path := filepath.Join(dir, otelName)
	b, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

type job struct {
	mu      sync.Mutex
	id      string
	status  string
	errMsg  string
	dir     string
	files   []string
	created time.Time
}

func (j *job) setStatus(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = s
}

func (j *job) setDir(d string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.dir = d
}

func (j *job) getDir() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dir
}

func (j *job) setFiles(files []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.files = append([]string(nil), files...)
}

func (j *job) getFiles() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.files...)
}

func (j *job) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "error"
	j.errMsg = err.Error()
}

func (j *job) snapshot() (status, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.errMsg
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
