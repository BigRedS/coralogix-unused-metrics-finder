# coralogix-unused-metrics-finder

**coralogix-unused-metrics-finder** is a tool for discovering unused metrics series in Coralogix, and generating OpenTelemetry config to block them.


---

## API key permissions

Use one **personal** or **team** API key for `--key`.

You will probably want these presets:

* `DataQuerying`
* `DataAnalytics`
* `Dashboards`
* `Alerts`
* `SLO`



## Build & run

```bash
go build -o bin/coralogix-unused-metrics-finder ./cmd/coralogix-unused-metrics-finder/
./bin/coralogix-unused-metrics-finder --region eu2 --key "$CX_API_KEY" --output-dir ./out
```

See `--help` for flags (`--usage-lookback-days`, `--usage-billing-calendar-months`, `--skip-billing`, `--skip-dashboards`, `--skip-alerts`, `--skip-slo`, etc.).

### Browser UI

Self-contained server under **`webui/`**: choose region, paste API key, run scan, download outputs (same files as `--output-dir`).

```bash
go run ./webui -listen localhost:8765
```

see `webui/README.md` for more info.

### Metrics whose series can't be retrieved

Very high-cardinality metrics sometimes can't be enumerated at all. The `/api/v1/series` query for them may be refused outright (the server-side series-analysis cap, `ViolationTypeTotalSeriesAnalyzed`), time out, or come back as a `500`. None of these aborts the scan: the metric name is recorded in **`series_fetch_failures`** in `metric_usage_summary.json` with a `category` and a reason, and the scan continues without it. Counts appear in `meta.series_fetch_failures_count` / `meta.series_fetch_timeouts_count`, as a stderr warning, and in the PDF.

| `category` | Meaning |
|------------|---------|
| `series_analysis_cap` | Coralogix refused the query: more series in the window than it will analyze at once. |
| `timeout` | The query didn't finish before `--timeout-sec`. |
| `server_error` | Coralogix answered `5xx`/`429` for this metric — commonly what the series endpoint does for a metric it can't enumerate. |
| `transport` | The request failed outside an HTTP status (connection reset, unreadable body). |

Such metrics are **excluded from used/unused classification and from the OTEL fragment** — their usage is *unknown*, not confirmed unused. To bring them into the scan, shorten **`--series-lookback-hours`**, or raise **`--timeout-sec`** for the timed-out ones.

Two things still fail the run, so a broken scan never masquerades as "everything is unused": statuses that mean the *request* was rejected rather than the metric (`400`, `401`, `402`, `403`, `404`, `405` — bad key, missing permission, wrong region), and more than **half** the metric names failing, whatever the reason.

If your API key lacks **Dashboards**, **Alerts**, or **SLO** access, pass **`--skip-dashboards`**, **`--skip-alerts`**, and/or **`--skip-slo`** so the scan skips those HTTP calls. Correlation (used vs unused, OTEL drops/strips) then considers only PromQL from the sources that ran; skipped modes append **`warnings`** in `metric_usage_summary.json`. Skipping **all three** makes every catalog series appear unused.


---

## Output files

If the API key has team-admin scope the tool first calls **`TeamService.ListTeams`** and uses the returned team name (sanitized to ASCII alphanumerics + `._-`) as a prefix on every output file, e.g. `MyTeam-metric_usage_summary.json`. If the call fails (typically `PermissionDenied` for narrower keys), filenames stay unprefixed and the rest of the scan continues normally — no extra flag needed.

| File | Description |
|------|-------------|
| `metric_usage_summary.json` | Full report: correlation results, warnings, `meta` (including `usage_lookback_days`, `series_with_billing_data`, `unused_series_with_billing`, `coralogix_internal_metric_names_skipped`). Unused entries here use optional nested `"billing": { "unit_usage": … }` when matched. |
| `metric_usage_unused_series.json` | Unused series only (alphabetically by full selector string). |
| **`metric_usage_unused_by_cost.json`** | Same unused series, **sorted by cost**, with **flat** billing fields on every row (easier for `jq` / tooling). |
| **`metric_usage_unused_by_cost.csv`** | Same data as the JSON cost file, as a spreadsheet-friendly CSV. |
| **`metric_usage_unused_by_metric.json`** | **Rollup**: one row per unused **`__name__`**, sorted by **`unit_usage_sum`** — sums billing fields over unused series that have CX data (see below). |
| **`metric_usage_unused_by_metric.csv`** | Same metric rollup as spreadsheet-friendly CSV. |
| **`metric_usage_all_by_metric.csv`** | One row per **`__name__`** across **both** used and unused catalog series: `metric_name`, `series_count` (distinct catalog series), `unit_usage_sum` (summed CX billing over the window). |
| **`metric_usage_otel_processors.yaml`** | Fragment for **otelcol-contrib**: drops metrics that are unused end-to-end, and strips label keys that appear only on unused series for partially-used metrics (see below). |
| **`metric_usage_report.pdf`** | Printable summary of the run: scan settings, headline numbers, top unused metrics by cost, and step-by-step instructions for applying the OTEL fragment. Same content is derivable from the other files — provided as a single human-readable artefact to share. |

### CX billing window (**`unit_usage`** sample span)

Billing is skipped when **`--skip-billing`** is set, or when both **`--usage-lookback-days`** is **`0`** and **`--usage-billing-calendar-months`** is **`0`**.

- **`--usage-lookback-days`** (default **`7`**) — rolling window of inclusive **UTC calendar days** ending at **today’s UTC date** (midnight-aligned). Example: on **2026-05-15**, **`7`** means **2026-05-09** through **2026-05-15** inclusive.
- **`--usage-billing-calendar-months N`** (**`N > 0`**) — last **`N`** **complete UTC calendar months**, excluding the current partial month: from the **first day** of month **`current − N`** through the **last day** of the **previous** month. If both rolling days and **`N`** are non-zero, **calendar months win** for the API request (CLI still accepts both flags).

The exact dates sent to Metrics Usage are recorded in **`metric_usage_summary.json`** → **`meta.usage_billing_utc_start_date`**, **`meta.usage_billing_utc_end_date`**, **`meta.usage_billing_calendar_months`**, and **`meta.usage_lookback_days`** (inclusive day count for whichever window ran).

### Why **`unit_usage`** often looks tiny

CX **`unit_usage`** is the Coralogix Unit cost of a given series. Values can be **fractional** by design. This tool also **attributes** one CX variation row to multiple Prometheus catalog series when labels only **subset**-match: usage is **split evenly** (**`billing_split_n`**), so each series sees **`1/N`** of that row — fine-grained and legitimately small.

For prioritization, prefer **`metric_usage_unused_by_metric.*`**, which adds **`unit_usage_sum`** per metric so you see aggregate attributed usage instead of thousands of thin per-series rows.

**Rollup semantics:** **`unit_usage_sum`** / **`bytes_volume_sum`** / **`sample_count_sum`** are sums of **per-series attributed** values (only rows with **`billing_present`**). They approximate total unused footprint for that metric name but are **not** a substitute for CX invoices. **`cardinality_sum`** sums CX-reported per-series cardinality figures (a heuristic, not “distinct labels across series”). **`unused_series_with_billing_count`** vs **`unused_series_count`** shows how many unused series lacked a billing match.

### OpenTelemetry Collector fragment (`metric_usage_otel_processors.yaml`)

Requires **otelcol-contrib** with the **filter** and **transform** processors. Merge the generated `processors:` block into your Collector config, then add both processor IDs to your metrics pipeline (recommended order: drop unused metrics first, then strip labels):

```yaml
service:
  pipelines:
    metrics:
      receivers: [prometheus, ...]
      processors:
        - memory_limiter
        - metrics_usage_drop_unused_metrics
        - metrics_usage_strip_unused_only_labels
        - batch
      exporters: [...]
```

Semantics:

- **`metrics_usage_drop_unused_metrics`** — `filter` with strict metric-name excludes: every series for that `__name__` was **unused** (no **scanned** dashboard, alert, or SLO matched any series for that metric in the window). If you used `--skip-dashboards`, `--skip-alerts`, or `--skip-slo`, “used” excludes references from skipped sources.
- **`metrics_usage_strip_unused_only_labels`** — `transform` OTTL on **datapoint** context: for metrics that still have **some** used series, emit `delete_key(attributes, "<key>") where metric.name == "<metric>"` for each label key that appears on at least one **unused** catalog series but **never** on any **used** catalog series for that metric. Prometheus scrape labels are assumed to live on datapoint attributes (typical **prometheusreceiver** layout).

If nothing qualifies, the file contains `processors: {}`. Review before production: stripping dimensions changes metric identity; the catalog may be incomplete if metrics hit `--series-limit-per-metric`.

### `metric_usage_unused_by_cost.json` — fields

Each element is one unused time series:

- **`metric_name`** — value of the `__name__` label.
- **`series`** — full canonical selector string (metric + sorted labels), same key used elsewhere in the report.
- **`labels`** — label map as JSON object.
- **`billing_present`** — `true` if CX Metrics Usage returned a matching row for this series over the configured billing window (**`--usage-lookback-days`** or **`--usage-billing-calendar-months`**).
- **`unit_usage`**, **`bytes_volume`**, **`sample_count`**, **`cardinality`**, **`days_in_range`** — from Coralogix when `billing_present` is true; otherwise numeric zeros.
- **`billing_split_n`** — if greater than `1`, one billing **variation** row applied to several catalog series, so usage was **divided evenly** across them (approximate per-series attribution).
