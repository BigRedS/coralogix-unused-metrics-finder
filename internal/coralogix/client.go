package coralogix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/region"
)

// HTTPStatusError is a non-2xx response from the Coralogix API. It carries the status code so
// callers can tell a failure of one request (5xx, 429, gateway timeout) from a failure of the
// whole configuration (401, 403, 400) without matching on message text. Error() keeps the
// original "<status>\n\n<replication block>" shape so diagnostics are unchanged.
type HTTPStatusError struct {
	Code   int
	Status string
	Detail string
}

func (e *HTTPStatusError) Error() string {
	if e.Detail == "" {
		return e.Status
	}
	return e.Status + "\n\n" + e.Detail
}

// SeriesAnalysisLimitError is returned by FetchSeriesForMetric when Coralogix rejects the
// /api/v1/series query with HTTP 422 ViolationTypeTotalSeriesAnalyzed — i.e. the metric has
// more underlying time series than the server is willing to analyze in one call. Callers
// can treat the affected metric as unanalyzable and continue with the rest of the scan.
type SeriesAnalysisLimitError struct {
	MetricName string
	Cause      error
}

func (e *SeriesAnalysisLimitError) Error() string {
	return fmt.Sprintf("series analysis limit exceeded for metric %q: %v", e.MetricName, e.Cause)
}

func (e *SeriesAnalysisLimitError) Unwrap() error { return e.Cause }

// SeriesTimeoutError is returned by FetchSeriesForMetric when the /api/v1/series query ran
// out of time instead of producing an answer: the HTTP client timeout fired, the request
// deadline expired, or an upstream gateway gave up (408/504). Metrics with enormous series
// counts do this reliably, so callers can record the metric as unanalyzable and carry on
// with the rest of the scan rather than failing the whole run.
type SeriesTimeoutError struct {
	MetricName string
	Cause      error
}

func (e *SeriesTimeoutError) Error() string {
	return fmt.Sprintf("series query timed out for metric %q: %v", e.MetricName, e.Cause)
}

func (e *SeriesTimeoutError) Unwrap() error { return e.Cause }

// isSeriesAnalysisLimit detects the server-side per-query series cap from the error message
// produced by c.get. The diagnostic body (included in the wrapped error) carries the
// ViolationTypeTotalSeriesAnalyzed marker.
func isSeriesAnalysisLimit(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "ViolationTypeTotalSeriesAnalyzed") {
		return true
	}
	return strings.Contains(s, "422") && strings.Contains(s, "series limit")
}

// isTimeout reports whether err is a "ran out of time" failure rather than a definitive
// answer from the server. Covers request/client deadlines (net.Error.Timeout, and the
// http.Client.Timeout error which does not always wrap context.DeadlineExceeded) plus the
// HTTP statuses a gateway returns when it stops waiting on Coralogix.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeouter interface{ Timeout() bool }
	if errors.As(err, &timeouter) && timeouter.Timeout() {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code == http.StatusGatewayTimeout || statusErr.Code == http.StatusRequestTimeout
	}
	return strings.Contains(err.Error(), "Client.Timeout exceeded")
}

type Client struct {
	APIHost     string
	MgmtBase    string
	MetricsBase string
	HTTP        *http.Client
	apiKey      string
}

func NewClient(apiHost, apiKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &Client{
		APIHost:     apiHost,
		MgmtBase:    region.MgmtOpenAPIV5Base(apiHost),
		MetricsBase: region.MetricsBase(apiHost),
		apiKey:      apiKey,
		HTTP: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) get(ctx context.Context, rawURL string, query url.Values) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w\n\n%s", err, formatHTTPReplication(req, nil, nil))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		diagBody, truncated, rerr := readResponseBodyLimited(resp, maxDiagnosticBody)
		if rerr != nil {
			return nil, fmt.Errorf("read error response body: %w\n\n%s", rerr, formatHTTPReplication(req, resp, nil))
		}
		if truncated {
			diagBody = append(diagBody, []byte(fmt.Sprintf(
				"\n... truncated after %d bytes (response body may be larger)", maxDiagnosticBody))...)
		}
		return nil, &HTTPStatusError{
			Code:   resp.StatusCode,
			Status: resp.Status,
			Detail: formatHTTPReplication(req, resp, diagBody),
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w\n\n%s", err, formatHTTPReplication(req, resp, nil))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type DashboardCatalogItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) FetchDashboardCatalog(ctx context.Context) ([]DashboardCatalogItem, error) {
	// OpenAPI v5: GET .../catalog/list (not .../catalog — that segment is mistaken for a dashboard id).
	u := c.MgmtBase + "/dashboards/dashboards/v1/catalog/list"
	body, err := c.get(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []DashboardCatalogItem `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) FetchDashboard(ctx context.Context, dashboardID string) (json.RawMessage, error) {
	u := c.MgmtBase + "/dashboards/dashboards/v1/" + url.PathEscape(dashboardID)
	return c.get(ctx, u, nil)
}

type alertDefsPage struct {
	AlertDefs  []json.RawMessage `json:"alertDefs"`
	Pagination struct {
		NextPageToken string `json:"nextPageToken"`
	} `json:"pagination"`
}

func (c *Client) FetchAllAlertDefs(ctx context.Context, pageSize int) ([]json.RawMessage, error) {
	if pageSize <= 0 {
		pageSize = 200
	}
	var all []json.RawMessage
	token := ""
	for {
		q := url.Values{}
		q.Set("pagination.pageSize", strconv.Itoa(pageSize))
		if token != "" {
			q.Set("pagination.pageToken", token)
		}
		u := c.MgmtBase + "/alerts/alerts/v3"
		body, err := c.get(ctx, u, q)
		if err != nil {
			return nil, err
		}
		var page alertDefsPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.AlertDefs...)
		token = page.Pagination.NextPageToken
		if token == "" {
			break
		}
	}
	return all, nil
}

func (c *Client) FetchSLOs(ctx context.Context) ([]json.RawMessage, error) {
	u := c.MgmtBase + "/slo/slos/v1"
	body, err := c.get(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		SLOs []json.RawMessage `json:"slos"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.SLOs, nil
}

type promAPIResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func (c *Client) FetchMetricNames(ctx context.Context, start, end time.Time) ([]string, error) {
	u := c.MetricsBase + "/api/v1/label/__name__/values"
	q := url.Values{}
	q.Set("start", formatUnix(start))
	q.Set("end", formatUnix(end))
	body, err := c.get(ctx, u, q)
	if err != nil {
		return nil, err
	}
	var resp promAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode __name__ values envelope JSON: %w\n\nHTTP 200 body (truncated):\n%s\n\n%s",
			err, truncate(string(body), 4096), c.formatGETReplication(u, q))
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("unexpected __name__ values status %q (HTTP was 2xx): %s\n\n%s",
			resp.Status, truncate(string(body), 4096), c.formatGETReplication(u, q))
	}
	var names []string
	if err := json.Unmarshal(resp.Data, &names); err != nil {
		return nil, fmt.Errorf("decode __name__ values data JSON: %w\n\nenvelope data field (truncated):\n%s\n\n%s",
			err, truncate(string(resp.Data), 4096), c.formatGETReplication(u, q))
	}
	return names, nil
}

func (c *Client) FetchSeriesForMetric(ctx context.Context, metricName string, start, end time.Time, limit int) ([]map[string]string, bool, error) {
	match := `{__name__=` + strconv.Quote(metricName) + `}`
	u := c.MetricsBase + "/api/v1/series"
	q := url.Values{}
	q.Add("match[]", match)
	q.Set("start", formatUnix(start))
	q.Set("end", formatUnix(end))
	q.Set("limit", strconv.Itoa(limit))
	body, err := c.get(ctx, u, q)
	if err != nil {
		if isSeriesAnalysisLimit(err) {
			return nil, false, &SeriesAnalysisLimitError{MetricName: metricName, Cause: err}
		}
		// Don't label a cancelled/expired caller context as a per-metric timeout — that's
		// scan-wide and the caller must not swallow it.
		if ctx.Err() == nil && isTimeout(err) {
			return nil, false, &SeriesTimeoutError{MetricName: metricName, Cause: err}
		}
		return nil, false, err
	}
	var resp promAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, fmt.Errorf("decode series envelope JSON for metric %q: %w\n\nHTTP 200 body (truncated):\n%s\n\n%s",
			metricName, err, truncate(string(body), 4096), c.formatGETReplication(u, q))
	}
	if resp.Status != "success" {
		return nil, false, fmt.Errorf("series query failed for %q with status %q (HTTP was 2xx): %s\n\n%s",
			metricName, resp.Status, truncate(string(body), 4096), c.formatGETReplication(u, q))
	}
	var series []map[string]string
	if err := json.Unmarshal(resp.Data, &series); err != nil {
		return nil, false, fmt.Errorf("decode series data JSON for metric %q: %w\n\ndata field (truncated):\n%s\n\n%s",
			metricName, err, truncate(string(resp.Data), 4096), c.formatGETReplication(u, q))
	}
	truncated := len(series) >= limit
	return series, truncated, nil
}

func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// AlertID extracts id from an alert definition JSON blob.
func AlertID(raw json.RawMessage) string {
	var m struct {
		ID             string `json:"id"`
		AlertVersionID string `json:"alertVersionId"`
	}
	_ = json.Unmarshal(raw, &m)
	if m.ID != "" {
		return m.ID
	}
	return m.AlertVersionID
}

// SLOID extracts id from an SLO JSON blob.
func SLOID(raw json.RawMessage) string {
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.ID
}
