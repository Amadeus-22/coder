# Monitoring

> [!NOTE]
> AI Gateway requires the [AI Governance Add-On](../ai-governance.md).
> As of Coder v2.32, deployments without the add-on will not be able to
> access AI Gateway.

AI Gateway records the last `user` prompt, token usage, model reasoning, and every tool invocation for each intercepted request.
Each capture is tied to a single "interception" that maps back to the authenticated Coder identity, which makes it possible to attribute spend and behavior.

![User Prompt logging](../../images/aibridge/grafana_user_prompts_logging.png)

![User Leaderboard](../../images/aibridge/grafana_user_leaderboard.png)

Coder provides an example Grafana dashboard that you can import as a starting point for your metrics.
Refer to the [Grafana dashboard README](../../../examples/monitoring/dashboards/grafana/aibridge/README.md).

These logs and metrics can be used to determine usage patterns, track costs, and evaluate tooling adoption.

## Health and readiness

A standalone AI Gateway exposes health endpoints on its data-plane listener:

| Endpoint   | Success condition                                                                     |
|------------|---------------------------------------------------------------------------------------|
| `/healthz` | The HTTP listener is serving.                                                         |
| `/readyz`  | The control connection to `coderd` is active and the initial provider load succeeded. |

`/readyz` returns HTTP 503 before the initial provider load succeeds and whenever the control connection to `coderd` is unavailable.
A successful empty provider list counts as a completed initial load.
`/healthz` continues to return HTTP 200 during a control-plane disconnection so that the process is not restarted while it attempts to reconnect.
Health and readiness requests bypass AI Gateway middleware and do not create trace spans.

The standalone Helm chart enables a `/healthz` liveness probe and a `/readyz` readiness probe by default.
The startup probe is disabled by default.
These settings remove disconnected or uninitialized replicas from the data-plane Service without restarting otherwise healthy replicas.

## Prometheus metrics

The embedded Gateway, standalone Gateway, and AI Gateway Proxy expose metrics from the process that handles their traffic.
Scrape every standalone replica separately because counters, gauges, Go runtime metrics, and process metrics are local to each process.

### Embedded Gateway and proxy metrics

The embedded Gateway exports metrics from the `coder server` Prometheus listener with the `coder_ai_gateway_` prefix.
AI Gateway Proxy exports metrics from the same listener with the `coder_ai_gateway_proxy_` prefix.
Refer to [provider configuration](./providers.md) for the provider reload lifecycle these metrics describe.

| Metric                                                                   | Type    | Labels                                     | Purpose                                                                                                                                    |
|--------------------------------------------------------------------------|---------|--------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| `coder_ai_gateway_provider_info`                                         | gauge   | `provider_name`, `provider_type`, `status` | One series per configured provider. Value is always `1`; the `status` label (`enabled`, `disabled`, `error`) carries the alertable signal. |
| `coder_ai_gateway_providers_last_reload_timestamp_seconds`               | gauge   |                                            | Unix timestamp of the last reload attempt, success or failure.                                                                             |
| `coder_ai_gateway_providers_last_reload_success_timestamp_seconds`       | gauge   |                                            | Unix timestamp of the last reload that successfully refreshed the pool.                                                                    |
| `coder_ai_gateway_proxy_provider_info`                                   | gauge   | `provider_name`, `provider_type`, `status` | The provider information reported by AI Gateway Proxy.                                                                                     |
| `coder_ai_gateway_proxy_providers_last_reload_timestamp_seconds`         | gauge   |                                            | Last reload attempt timestamp in AI Gateway Proxy.                                                                                         |
| `coder_ai_gateway_proxy_providers_last_reload_success_timestamp_seconds` | gauge   |                                            | Last successful reload timestamp in AI Gateway Proxy.                                                                                      |
| `coder_ai_gateway_proxy_connect_sessions_total`                          | counter | `type` (`mitm`, `tunneled`)                | CONNECT sessions established by AI Gateway Proxy.                                                                                          |
| `coder_ai_gateway_proxy_mitm_requests_total`                             | counter | `provider`                                 | MITM requests handled by AI Gateway Proxy.                                                                                                 |
| `coder_ai_gateway_proxy_inflight_mitm_requests`                          | gauge   | `provider`                                 | In-flight MITM requests.                                                                                                                   |
| `coder_ai_gateway_proxy_mitm_responses_total`                            | counter | `code`, `provider`                         | MITM responses by HTTP status code.                                                                                                        |

> [!IMPORTANT]
> The embedded Gateway metric prefix changed from `coder_aibridged_*` to `coder_ai_gateway_*`, and the proxy prefix changed from `coder_aibridgeproxyd_*` to `coder_ai_gateway_proxy_*`.
> The legacy names are emitted with identical values during the v2.35 and v2.36 deprecation window and are planned for removal in v2.37.
> Migrate dashboards and alerts to the new names.
> Do not relabel new names back to old names while both are emitted because this creates duplicate legacy series in the same scrape.
> After the legacy names are removed, use `metric_relabel_configs` only if you need a temporary compatibility bridge:
>
> ```yaml
> metric_relabel_configs:
>   # Proxy rule must come first; the gateway regex below also matches proxy metrics.
>   - source_labels: [__name__]
>     regex: 'coder_ai_gateway_proxy_(.*)'
>     target_label: __name__
>     replacement: 'coder_aibridgeproxyd_${1}'
>   - source_labels: [__name__]
>     regex: 'coder_ai_gateway_(.*)'
>     target_label: __name__
>     replacement: 'coder_aibridged_${1}'
> ```

The complete embedded metric list is available in the [Prometheus reference](../../admin/integrations/prometheus.md).

### Standalone Gateway metrics

Enable the standalone metrics listener with `CODER_PROMETHEUS_ENABLE=true` and set its bind address with `CODER_PROMETHEUS_ADDRESS`.
The command default is `127.0.0.1:2112`.
The listener is unauthenticated, so expose it only to your monitoring network.

The standalone registry currently emits AI Gateway metric names without the `coder_ai_gateway_` prefix:

| Category        | Metric families                                                                                                  |
|-----------------|------------------------------------------------------------------------------------------------------------------|
| Interceptions   | `interceptions_total`, `interceptions_inflight`, `interceptions_duration_seconds`, `passthrough_total`           |
| Usage           | `prompts_total`, `tokens_total`, `injected_tool_invocations_total`, `non_injected_tool_selections_total`         |
| Circuit breaker | `circuit_breaker_state`, `circuit_breaker_trips_total`, `circuit_breaker_rejects_total`                          |
| Provider keys   | `key_pool_state`, `key_pool_state_transitions_total`, `key_pool_exhaustions_total`, `key_pool_failover_attempts` |
| Provider reload | `provider_info`, `providers_last_reload_timestamp_seconds`, `providers_last_reload_success_timestamp_seconds`    |

Histograms also emit the standard `_bucket`, `_sum`, and `_count` series.
The registry also includes standard `go_*`, `process_*`, and Prometheus handler metrics.
Account for the different names when you reuse dashboards or alerts created for the embedded Gateway.

For a process that you manage directly, scrape the configured listener, for example:

```console
curl http://127.0.0.1:2112/metrics
```

The standalone Helm chart enables metrics and binds the listener to `0.0.0.0:2112` by default.
The chart declares a named `metrics` container port but does not expose it through the data-plane Service or create monitoring discovery resources.
Configure pod-based discovery so that Prometheus scrapes each replica, for example:

```yaml
coder:
  podAnnotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "2112"
```

Every chart pod has the `app.kubernetes.io/component: ai-gateway` annotation, which annotation-aware Kubernetes discovery can use.
A `PodMonitor` selector matches labels instead of annotations.
Match the chart's `app.kubernetes.io/name` and `app.kubernetes.io/instance` pod labels, or add dedicated labels with `coder.podLabels`.
You can create the `PodMonitor` with `extraTemplates`.
If your monitoring stack uses a `ServiceMonitor`, create a separate Service that exposes the `metrics` container port because the chart's data-plane Service exposes only the HTTP traffic port.

### Suggested alerts

For the embedded Gateway, alert on any provider entering a non-`enabled` status:

```promql
sum by (provider_name, status) (coder_ai_gateway_provider_info{status!="enabled"}) > 0
```

For standalone replicas, use the unprefixed metric and preserve a pod or instance label in the alert:

```promql
sum by (instance, provider_name, status) (provider_info{status!="enabled"}) > 0
```

Alert when the embedded provider reload loop is firing but failing to refresh the pool for longer than a few minutes:

```promql
(coder_ai_gateway_providers_last_reload_timestamp_seconds
  - coder_ai_gateway_providers_last_reload_success_timestamp_seconds) > 300
```

Use `providers_last_reload_timestamp_seconds` and `providers_last_reload_success_timestamp_seconds` for the equivalent standalone alert.
Use the `coder_ai_gateway_proxy_*` metrics when you alert on AI Gateway Proxy.

## Logs

### Standalone operational logs

Standalone replicas use the standard Coder logging settings:

- `CODER_LOGGING_HUMAN`
- `CODER_LOGGING_JSON`
- `CODER_LOGGING_STACKDRIVER`
- `CODER_LOG_FILTER`
- `CODER_VERBOSE`

Configure these variables on every replica or through `coder.env` in the standalone Helm chart.
Aggregate logs from every replica and retain pod identity so that you can correlate connection, provider reload, and request failures with per-replica metrics and traces.
Refer to the [`coder ai-gateway start` logging options](../../reference/cli/ai-gateway_start.md#--log-filter) for details.

### Structured interception logs

AI Gateway can emit a structured log for every interception record to an external SIEM or observability platform.
The `CODER_AI_GATEWAY_STRUCTURED_LOGGING` setting belongs to `coderd`, including when standalone replicas serve the AI traffic.
Standalone replicas send interception records to `coderd`, which writes the structured logs to the Coder server log output.
Do not set `CODER_AI_GATEWAY_STRUCTURED_LOGGING` on standalone pods because `coder ai-gateway start` does not consume it.
Refer to [structured logging](./setup.md#structured-logging) for configuration and record types.

## Exporting Data

AI Gateway interception data can be exported for external analysis, compliance reporting, or integration with log aggregation systems.

### REST API

You can retrieve AI Gateway sessions via the Coder API, with filtering and pagination support.

```sh
curl -X GET "https://coder.example.com/api/v2/ai-gateway/sessions" \
  -H "Coder-Session-Token: $CODER_SESSION_TOKEN"
```

Available query filters:

- `client` - Filter by client name.
  <details>
  <summary>Possible <code>client</code> values</summary>

  > [!NOTE]
  > Client classification is done on best effort basis using the `User-Agent` header;
  not all clients send these headers in an easily-identifiable manner.

  - `Claude Code`
  - `Codex`
  - `Zed`
  - `GitHub Copilot (VS Code)`
  - `GitHub Copilot (CLI)`
  - `Kilo Code`
  - `Coder Agents`
  - `Mux`
  - `Cursor`
  - `OpenCode`
  - `Unknown`

  </details><br>
- `initiator` - Filter by user ID or username
- `provider` - Filter by AI provider (e.g., `openai`, `anthropic`)
- `model` - Filter by model name
- `started_after` - Filter sessions after a timestamp
- `started_before` - Filter sessions before a timestamp

See the [API documentation](../../reference/api/aigateway.md) for full details.

## Data Retention

AI Gateway data is retained for **60 days by default**. Configure the retention
period to balance storage costs with your organization's compliance and analysis
needs.

For configuration options and details, see [Data Retention](./setup.md#data-retention)
in the AI Gateway setup guide.

## Tracing

AI Gateway supports tracing through [OpenTelemetry](https://opentelemetry.io/) for request processing, upstream API calls, and MCP server interactions.
Embedded Gateway spans are emitted by the `coder server` process.
Standalone spans are emitted independently by every replica with the service name `coder-ai-gateway`.

### Enable tracing

For the embedded Gateway, configure tracing on `coder server` with `CODER_TRACE_ENABLE=true` or `--trace`.

For standalone replicas, set the tracing variables on every process or through `coder.env` in the Helm chart:

- `CODER_TRACE_ENABLE`
- `CODER_TRACE_HONEYCOMB_API_KEY`
- `CODER_TRACE_DATADOG`
- `CODER_TRACE_LOGS`

For example:

```yaml
coder:
  env:
    - name: CODER_TRACE_ENABLE
      value: "true"
    - name: CODER_TRACE_HONEYCOMB_API_KEY
      valueFrom:
        secretKeyRef:
          name: ai-gateway-tracing
          key: honeycomb-api-key
```

Configure your trace backend to ingest spans from every replica.
Standalone replicas also create an HTTP server span named `<method> <path>` around every data-plane request.
Health and readiness requests do not create spans.

### Traced operations

AI Gateway creates spans for the following operations:

| Span name                                   | Description                                          |
|---------------------------------------------|------------------------------------------------------|
| `CachedBridgePool.Acquire`                  | Acquiring a request bridge instance from the pool    |
| `Intercept`                                 | Top-level span for processing an intercepted request |
| `Intercept.CreateInterceptor`               | Creating the request interceptor                     |
| `Intercept.ProcessRequest`                  | Processing the request through the bridge            |
| `Intercept.ProcessRequest.Upstream`         | Forwarding the request to the upstream AI provider   |
| `Intercept.ProcessRequest.ToolCall`         | Executing a tool call requested by the AI model      |
| `Intercept.RecordInterception`              | Creating the interception record                     |
| `Intercept.RecordPromptUsage`               | Recording prompt and message data                    |
| `Intercept.RecordTokenUsage`                | Recording token consumption                          |
| `Intercept.RecordToolUsage`                 | Recording tool and function calls                    |
| `Intercept.RecordModelThought`              | Recording model reasoning                            |
| `Intercept.RecordInterceptionEnded`         | Recording the interception as completed              |
| `Passthrough`                               | Forwarding a non-intercepted provider request        |
| `ServerProxyManager.Init`                   | Initializing MCP server proxy connections            |
| `StreamableHTTPServerProxy.Init`            | Setting up HTTP-based MCP server proxies             |
| `StreamableHTTPServerProxy.Init.fetchTools` | Fetching available tools from MCP servers            |

Example trace of an interception using a Jaeger backend:

![Trace of interception](../../images/aibridge/jaeger_interception_trace.png)

### Capture logs in traces

> [!NOTE]
> Enabling log capture may generate a large volume of trace events.

Set `CODER_TRACE_LOGS=true` with tracing enabled to include log messages as trace events:

```sh
export CODER_TRACE_ENABLE=true
export CODER_TRACE_LOGS=true
```

For the embedded Gateway, you can also start Coder with `coder server --trace --trace-logs`.
For standalone replicas, configure both variables on every process that should capture logs.
