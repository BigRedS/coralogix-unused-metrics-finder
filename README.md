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

Cost data is **opt-in**: add **`--billing`** for `unit_usage` figures and the cost-ranked outputs (see [CX billing is opt-in](#cx-billing-is-opt-in---billing)).

See `--help` for flags (`--billing`, `--billing-allow-partial`, `--usage-lookback-days`, `--usage-billing-calendar-months`, `--grpc-host`, `--skip-dashboards`, `--skip-alerts`, `--skip-slo`, etc.).

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

### REST and gRPC endpoints

The scan uses two endpoints per account, and they are **not the same host**:

| | host | used for |
|---|---|---|
| REST | `api.<domain>` | metric names, series, dashboards, alerts, SLOs |
| gRPC | `ng-api-grpc.<domain>` | CX billing (`unit_usage`) and team-name lookup |

This matters because on eu1, us1, ap1 and ap2 the REST host does **not** negotiate ALPN, and grpc-go rejects such connections outright — every billing call fails at the TLS handshake with `credentials: cannot check peer: missing selected ALPN property`. `ng-api-grpc.<domain>` negotiates h2 on every region, so that is what the tool dials; `region.GRPCHost` derives it from `--region`. Override with **`--grpc-host`** if a cluster differs.

Do not "fix" an ALPN failure with `GRPC_ENFORCE_ALPN_ENABLED=false`: it has to be exported before the process starts (grpc-go reads it at package init), it applies to every connection in the process, it drops the guarantee that the peer intends to speak HTTP/2, and grpc-go warns it will stop being honoured in a future release. Point the tool at the right host instead.

### Memory use on large accounts

The scan holds the whole series catalog in memory while it correlates, so peak memory scales with **distinct series**, not metric names. Roughly **0.5–1.5 KB per series** after interning (wider label sets cost more), so a 3M-series account needs a few GB. On a memory-pressured machine the process can be killed outright — on macOS that appears as `Killed: 9` with no Go panic. `/usr/bin/time -l` reports the actual peak (`maximum resident set size`).

Two things keep it as low as it is, both worth knowing before changing them:

- Label names and values are **interned** — one retained copy each, rather than one per series (`meta.distinct_label_strings_retained` reports how many distinct strings the catalog holds). Worth ~30% of catalog memory.
- The JSON outputs are **streamed row by row** rather than built with `json.MarshalIndent`, which would hold the whole document — twice — before writing. Worth ~4× on the peak of the output phase. `internal/report/jsonstream.go` produces byte-identical output to the encoder; a test pins that.

If a scan still won't fit, reduce **`--series-lookback-hours`** (fewer series in the window) or **`--workers`** (fewer concurrent responses being decoded).

### Warnings are folded, not repeated

Steps that loop over many items — one request per dashboard, one billing lookup per metric — used to emit one warning per failed item, so a single root cause (a dead connection, an expired key) produced thousands of copies of the same paragraph. Warnings sharing a summary line are now folded into one entry that names the cause once and lists the affected subjects:

```
billing lookup failed for 1,204 metrics, all with the same error: rpc error: code = Unavailable
desc = connection error: … missing selected ALPN property — affected: "FE_amount_cx_count",
"a_metric", … (+1,199 more)
```

A failure affecting a single item keeps its full detail, including the HTTP replication block, since that detail is the diagnostic.

If your API key lacks **Dashboards**, **Alerts**, or **SLO** access, pass **`--skip-dashboards`**, **`--skip-alerts`**, and/or **`--skip-slo`** so the scan skips those HTTP calls. Correlation (used vs unused, OTEL drops/strips) then considers only PromQL from the sources that ran; skipped modes append **`warnings`** in `metric_usage_summary.json`. Skipping **all three** makes every catalog series appear unused.

### Run via the `cx` CLI (OAuth, no API key)

Instead of `--region` + `--key`, pass **`--profile <name>`** to fetch dashboards, alerts, SLOs and metrics through the [`cx` CLI](https://github.com/coralogix/cx) using its OAuth profiles (`~/.cx/profiles/*.toml`, credentials in the OS keychain). No API key is required.

```bash
cx profiles add my-team        # one-time OAuth login
./bin/coralogix-unused-metrics-finder --profile my-team --output-dir ./out
```

The `cx` binary must be on `PATH`. Each `cx` invocation is bounded by `--timeout-sec` (default 120s); a metric whose series query exceeds it is recorded as unanalyzable and skipped.

**Definition fetches retry rather than skip.** The CLI has no bulk endpoint for alert or SLO definitions, so profile mode issues one `cx <res> get` per resource — thousands of calls on a large tenant, where a transient API `500` on some individual call is close to inevitable. Those calls are retried four times with doubling backoff (2s, 4s, 8s), and the scan **aborts** if a definition is still unreachable. It deliberately does not skip the resource the way an unanalyzable metric is skipped: an alert the scan never read is a place a metric might be in use, so dropping it would report metrics as unused that aren't — the one answer this tool must not get wrong. Errors that retrying cannot fix (bad profile, `401`/`403`/`404`) fail immediately.

**Billing is hybrid.** The CLI has no per-metric usage surface, so cost columns (`unit_usage`, `bytes_volume`, cardinality, `$` savings) are blank in profile mode **unless** you also pass **`--billing`** together with **`--key`** (and **`--region`**); the tool then runs the gRPC Metrics Usage path for cost data while everything else goes through the CLI. `--billing` without a key warns and continues without cost data.

**Limitations in CLI mode:**
- `cx metrics search` has no time-window filter, so the metric-name list isn't window-scoped the way the direct API is.
- Series are enumerated with `cx metrics query-range` over the lookback window (no native `/api/v1/series`); very high-cardinality metrics can be slow/memory-heavy on large tenants and may be skipped via the timeout.
- `cx dashboards catalog` is broken on some tenants and returns an empty list — dashboard correlation is then skipped for that run.
- Team-name filename prefixing (gRPC) is skipped without a `--key`.


---

## Output files

If the API key has team-admin scope the tool first calls **`TeamService.ListTeams`** and uses the returned team name (sanitized to ASCII alphanumerics + `._-`) as a prefix on every output file, e.g. `MyTeam-metric_usage_summary.json`. If the call fails (typically `PermissionDenied` for narrower keys), filenames stay unprefixed and the rest of the scan continues normally — no extra flag needed.

| File | Description |
|------|-------------|
| `metric_usage_summary.json` | Full report: correlation results, warnings, `meta` (including `usage_lookback_days`, `series_with_billing_data`, `unused_series_with_billing`, `coralogix_internal_metric_names_skipped`). Unused entries here use optional nested `"billing": { "unit_usage": … }` when matched. |
| `metric_usage_unused_series.json` | Unused series only (alphabetically by full selector string). |
| **`metric_usage_unused_by_cost.json`** | Same unused series, **sorted by cost**, with **flat** billing fields on every row (easier for `jq` / tooling). **`--billing` only** — see below. |
| **`metric_usage_unused_by_cost.csv`** | Same data as the JSON cost file, as a spreadsheet-friendly CSV. **`--billing` only.** |
| **`metric_usage_unused_by_metric.json`** | **Rollup**: one row per unused **`__name__`**, sorted by **`unit_usage_sum`** — sums billing fields over unused series that have CX data (see below). |
| **`metric_usage_unused_by_metric.csv`** | Same metric rollup as spreadsheet-friendly CSV. |
| **`metric_usage_all_by_metric.csv`** | One row per **`__name__`** across **both** used and unused catalog series: `metric_name`, `series_count` (distinct catalog series), `unit_usage_sum` (summed CX billing over the window). |
| **`metric_usage_otel_processors.yaml`** | Fragment for **otelcol-contrib**: drops metrics that are unused end-to-end, and strips label keys that appear only on unused series for partially-used metrics (see below). |
| **`metric_usage_report.pdf`** | Printable summary of the run: scan settings, headline numbers, top unused metrics by cost, and step-by-step instructions for applying the OTEL fragment. Same content is derivable from the other files — provided as a single human-readable artefact to share. |

### CX billing is opt-in (**`--billing`**)

**Billing is off by default.** Pass **`--billing`** to fetch CX cost data (`unit_usage`, `bytes_volume`, `sample_count` per series). It is opt-in because the lookup is slow — one gRPC call per metric name — and because its two per-series outputs, `metric_usage_unused_by_cost.json` and `.csv`, are by far the largest files the tool writes.

Without `--billing` you still get the whole point of the tool: which series are unused, the per-metric rollups, and the OTEL fragment. What you lose is what those series **cost**, so nothing is ranked or scored by spend.

When there is no billing data — `--billing` not given, or the lookup returned nothing — the two cost files are **not written at all** (with every cost column empty, the "by cost" order degrades to the series name and they become bulky duplicates of `metric_usage_unused_series.json`), and the PDF prints `not collected` / `unavailable` in place of cost figures rather than a `0` that would read as measured. The reason appears in `warnings` in `metric_usage_summary.json` either way.

**`--skip-billing`** is retained but no longer needed: it forces billing off, which is now the default.

#### Billing coverage

Billing is fetched with one call per metric name, and some of those calls can fail while others succeed. That matters more than it sounds: **a metric whose lookup failed contributes no usage, so it looks free** — it sinks to the bottom of every cost ranking and the totals understate reality, while the output still reads like a complete answer.

So coverage is measured (`meta.billing_metric_lookups_attempted` / `_succeeded`) and enforced: at least **90%** of lookups must succeed before cost figures are reported. Below that, cost figures and the cost-ranked files are **withheld**, with the coverage and the reason in `warnings` and in the PDF. Pass **`--billing-allow-partial`** to get them anyway — they are then labelled as a **lower bound** in the PDF and `meta.billing_partial_accepted` records the choice.

The window flags below apply **only** with `--billing`:

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
