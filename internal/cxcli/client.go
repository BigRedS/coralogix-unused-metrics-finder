// Package cxcli implements scan.Source by shelling out to the Coralogix `cx`
// CLI (https://github.com/coralogix/cx). Unlike the API-key-backed
// internal/coralogix client, it authenticates via the CLI's OAuth profiles
// (~/.cx/profiles/*.toml, credentials in the OS keychain) — no API key needed.
//
// Every command is run with `--profile <name> --read-only -o json`. The CLI
// writes progress text to stderr and clean JSON to stdout, so only stdout is
// parsed.
//
// Known limitations vs. the direct API (documented on the --profile flag):
//   - `cx metrics search` has no time-window filter, so FetchMetricNames
//     ignores start/end and returns the full current catalog.
//   - Per-series data comes from `cx metrics query-range` (no native
//     /api/v1/series endpoint); high-cardinality metrics may error or time out
//     and are reported as unanalyzable rather than aborting the scan.
//   - `cx dashboards catalog` is server-side broken on some tenants and yields
//     an empty list; dashboard correlation is then skipped for that run.
//
// Definition fetches are one CLI call per alert, SLO and dashboard, so a large
// tenant makes thousands of calls and a transient API 500 on any one of them is
// likely. Those calls are retried with backoff (see runRetrying) rather than
// skipped, because a definition the scan never read is a place a metric could
// have been in use.
package cxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
)

// defaultStep is the query-range resolution used to enumerate a metric's series.
// It controls samples-per-series, not the number of series; a coarse value keeps
// the response small while still covering the lookback window.
const defaultStep = time.Hour

// defaultTimeout caps each cx invocation when none is configured.
const defaultTimeout = 2 * time.Minute

// Definition fetches (alerts, SLOs, dashboards) are retried before the scan
// gives up. Unlike an unanalyzable metric, a definition cannot be skipped: each
// one is a place a metric might be used, so dropping it would silently promote a
// used metric to "unused" — the one answer the tool must not get wrong. The API
// returns transient 500s often enough on large tenants that a single flaky call
// should not end a long run, so we pause and retry, and abort only if the
// resource is still unreachable after every attempt.
const (
	defaultRetryAttempts = 4
	defaultRetryDelay    = 2 * time.Second
)

// Runner executes a `cx` subcommand (the profile/output flags are appended by
// the caller) and returns stdout. It is injectable so tests need no real CLI.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

type Client struct {
	profile       string
	host          string
	step          time.Duration
	timeout       time.Duration
	retryAttempts int
	retryDelay    time.Duration
	onRetry       func(args []string, attempt int, err error)
	run           Runner
}

// Option configures a Client.
type Option func(*Client)

// WithRunner overrides the command runner (used in tests).
func WithRunner(r Runner) Option { return func(c *Client) { c.run = r } }

// WithHost sets the host string reported in scan metadata (e.g. the resolved
// API host derived from --region). Defaults to "cx-profile:<name>".
func WithHost(h string) Option {
	return func(c *Client) {
		if h != "" {
			c.host = h
		}
	}
}

// WithStep sets the query-range step used by FetchSeriesForMetric.
func WithStep(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.step = d
		}
	}
}

// WithTimeout caps each `cx` invocation. A command exceeding it is killed; for
// FetchSeriesForMetric that surfaces as an unanalyzable metric (the scan skips
// it and continues) so one high-cardinality metric can't hang the whole run.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithRetry sets how many times a definition fetch is attempted and the initial
// backoff between attempts (it doubles each time). Attempts below 1 are ignored.
func WithRetry(attempts int, delay time.Duration) Option {
	return func(c *Client) {
		if attempts >= 1 {
			c.retryAttempts = attempts
		}
		if delay >= 0 {
			c.retryDelay = delay
		}
	}
}

// WithRetryNotify registers a callback invoked before each retry pause, so the
// caller can tell the user why the scan appears to have stalled.
func WithRetryNotify(f func(args []string, attempt int, err error)) Option {
	return func(c *Client) { c.onRetry = f }
}

// NewClient returns a Source backed by the `cx` CLI for the given profile.
func NewClient(profile string, opts ...Option) *Client {
	c := &Client{
		profile:       profile,
		step:          defaultStep,
		timeout:       defaultTimeout,
		retryAttempts: defaultRetryAttempts,
		retryDelay:    defaultRetryDelay,
	}
	for _, o := range opts {
		o(c)
	}
	if c.run == nil {
		c.run = c.execCX
	}
	if c.host == "" {
		c.host = "cx-profile:" + profile
	}
	return c
}

// Host returns the metadata host identifier.
func (c *Client) Host() string { return c.host }

// execCX runs `cx <args> --profile <p> --read-only -o json`, returning stdout.
// Each invocation is bounded by c.timeout so a single slow command (e.g. a
// query-range over a very high-cardinality metric) cannot hang the scan.
func (c *Client) execCX(ctx context.Context, args ...string) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	full := make([]string, 0, len(args)+5)
	full = append(full, args...)
	full = append(full, "--profile", c.profile, "--read-only", "-o", "json")

	cmd := exec.CommandContext(ctx, "cx", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("cx %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("cx %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// runRetrying is run with backoff for calls whose result the scan cannot do
// without. It pauses between attempts and returns the last error if none
// succeed, so an unrecoverable resource still aborts the scan rather than
// quietly leaving a gap in the correlation set.
//
// The series query-range path deliberately does not use this: it has its own
// skip-and-continue semantics, and retrying a command that just consumed the
// full timeout would multiply the cost of every high-cardinality metric.
func (c *Client) runRetrying(ctx context.Context, args ...string) ([]byte, error) {
	attempts := c.retryAttempts
	if attempts < 1 {
		attempts = 1
	}
	delay := c.retryDelay

	var lastErr error
	for attempt := 1; ; attempt++ {
		out, err := c.run(ctx, args...)
		if err == nil {
			return out, nil
		}
		lastErr = err

		// A cancelled caller context is scan-wide; retrying cannot help.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= attempts || !isRetryable(err) {
			return nil, lastErr
		}
		if c.onRetry != nil {
			c.onRetry(args, attempt, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// permanentMarkers appear in cx errors that no amount of retrying will fix —
// a missing profile, rejected credentials, or a resource that is simply gone.
// Failing fast on these keeps a misconfigured run from sitting through every
// backoff before reporting what was wrong from the first attempt.
var permanentMarkers = []string{
	"Configuration error",
	"not found",
	"(401)",
	"(403)",
	"(404)",
}

func isRetryable(err error) bool {
	msg := err.Error()
	for _, m := range permanentMarkers {
		if strings.Contains(msg, m) {
			return false
		}
	}
	return true
}

// FetchMetricNames lists every metric name. The cx CLI has no time-window
// filter, so start/end are ignored (see package doc).
func (c *Client) FetchMetricNames(ctx context.Context, _, _ time.Time) ([]string, error) {
	out, err := c.runRetrying(ctx, "metrics", "search", "--name", "*")
	if err != nil {
		return nil, fmt.Errorf("list metric names: %w", err)
	}
	var names []string
	if err := json.Unmarshal(out, &names); err != nil {
		return nil, fmt.Errorf("decode metric names: %w", err)
	}
	return names, nil
}

// FetchDashboardCatalog returns the dashboard catalog. On tenants where the CLI
// catalog endpoint is broken it returns an empty list (the CLI exits 0 with []).
func (c *Client) FetchDashboardCatalog(ctx context.Context) ([]coralogix.DashboardCatalogItem, error) {
	out, err := c.runRetrying(ctx, "dashboards", "catalog")
	if err != nil {
		return nil, fmt.Errorf("dashboard catalog: %w", err)
	}
	var items []coralogix.DashboardCatalogItem
	if err := json.Unmarshal(out, &items); err == nil {
		return items, nil
	}
	// Tolerate an object envelope ({"items": [...]}).
	var wrap struct {
		Items []coralogix.DashboardCatalogItem `json:"items"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("decode dashboard catalog: %w", err)
	}
	return wrap.Items, nil
}

// FetchDashboard returns one dashboard's full JSON definition.
func (c *Client) FetchDashboard(ctx context.Context, dashboardID string) (json.RawMessage, error) {
	out, err := c.runRetrying(ctx, "dashboards", "get", dashboardID)
	if err != nil {
		return nil, fmt.Errorf("dashboard %s: %w", dashboardID, err)
	}
	return json.RawMessage(out), nil
}

// FetchAllAlertDefs lists alerts then fetches each full definition. pageSize is
// ignored (the CLI paginates internally). Each definition is wrapped as
// {"id":..,"definition":..} so coralogix.AlertID keys it and
// promqlextract.ExtractFromJSON still finds the nested PromQL.
func (c *Client) FetchAllAlertDefs(ctx context.Context, _ int) ([]json.RawMessage, error) {
	out, err := c.runRetrying(ctx, "alerts", "list")
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	ids, err := decodeIDList(out)
	if err != nil {
		return nil, fmt.Errorf("decode alerts list: %w", err)
	}
	defs := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		def, err := c.runRetrying(ctx, "alerts", "get", id)
		if err != nil {
			return nil, fmt.Errorf("get alert %s: %w", id, err)
		}
		defs = append(defs, wrapWithID(id, def))
	}
	return defs, nil
}

// FetchSLOs lists SLOs then fetches each full definition, wrapped like alerts.
func (c *Client) FetchSLOs(ctx context.Context) ([]json.RawMessage, error) {
	out, err := c.runRetrying(ctx, "slos", "list")
	if err != nil {
		return nil, fmt.Errorf("list slos: %w", err)
	}
	ids, err := decodeIDList(out)
	if err != nil {
		return nil, fmt.Errorf("decode slos list: %w", err)
	}
	defs := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		def, err := c.runRetrying(ctx, "slos", "get", id)
		if err != nil {
			return nil, fmt.Errorf("get slo %s: %w", id, err)
		}
		defs = append(defs, wrapWithID(id, def))
	}
	return defs, nil
}

// FetchSeriesForMetric enumerates a metric's series via a range query over the
// window, unioning the label set of every returned series. The limit is unused
// (query-range has no row cap), so truncated is always false. A command failure
// is reported as *coralogix.SeriesAnalysisLimitError so the scan records the
// metric as unanalyzable and continues instead of aborting.
func (c *Client) FetchSeriesForMetric(ctx context.Context, metricName string, start, end time.Time, _ int) ([]map[string]string, bool, error) {
	selector := "{__name__=" + strconv.Quote(metricName) + "}"
	out, err := c.run(ctx, "metrics", "query-range", selector,
		"--start", start.UTC().Format(time.RFC3339),
		"--end", end.UTC().Format(time.RFC3339),
		"--step", stepArg(c.step),
	)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &coralogix.SeriesAnalysisLimitError{MetricName: metricName, Cause: err}
	}
	var rows []struct {
		Metric map[string]string `json:"metric"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, false, fmt.Errorf("decode series for %q: %w", metricName, err)
	}
	series := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		if r.Metric != nil {
			series = append(series, r.Metric)
		}
	}
	return series, false, nil
}

// decodeIDList parses a `cx <res> list -o json` array into the list of ids.
func decodeIDList(out []byte) ([]string, error) {
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listed))
	for _, item := range listed {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

// wrapWithID embeds a known id alongside a raw definition so downstream ID
// extraction works regardless of the get-command's JSON shape.
func wrapWithID(id string, def []byte) json.RawMessage {
	out := make([]byte, 0, len(def)+len(id)+24)
	out = append(out, `{"id":`...)
	out = append(out, strconv.Quote(id)...)
	out = append(out, `,"definition":`...)
	out = append(out, def...)
	out = append(out, '}')
	return out
}

// stepArg renders a duration as a cx-compatible step like "3600s".
func stepArg(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10) + "s"
}
