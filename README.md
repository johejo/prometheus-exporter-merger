# prometheus-exporter-merger

## Description

An alternative proxy for prometheus exporters.

Provides the feature to merge multiple upstream exporters into a single exporter.

## Merge behavior

- The exporter's own labels and the listener's `commonLabels` are spliced into every sample line.
- `# HELP` and `# TYPE` directives are emitted once per metric family: the first upstream exporter to define one wins.
- A later exporter's conflicting description or type for the same family is discarded silently, while its samples are still merged in.
- All upstream response headers are checked before any response body is emitted. A connection error, timeout, or non-2xx response from any upstream makes the merged request fail with `502 Bad Gateway`.

## Self metrics

The self-metrics listener exposes the following per-upstream metrics in addition to process metrics:

- `exporter_merger_upstream_requests_total{listener,upstream}`
- `exporter_merger_upstream_request_failures_total{listener,upstream}`
- `exporter_merger_upstream_request_duration_seconds{listener,upstream}`

`upstream_request_duration_seconds` measures the complete request, through consuming or closing its response body.

## Install

```
go install github.com/johejo/prometheus-exporter-merger@latest
```

## Usage

```
Usage of prometheus-exporter-merger:
  -config string
        configuration file path (default "config.yaml")
  -expand-env
        expand environment variables in config
  -log-level string
        logging level: debug, info, warn, error (default "info")
  -self-metrics-address string
        listen address for self metrics (default ":9716")
  -self-metrics-expose-metadata
        expose self metrics metadata (default true)
```

## Example

See ./testdata/config.yaml

An optional duration string such as `timeout: 5s` sets the timeout independently for each upstream. A missing or zero timeout means no per-upstream timeout.

Configuration is validated before listeners start; unknown fields, invalid listener or exporter settings, and conflicting labels are rejected.
