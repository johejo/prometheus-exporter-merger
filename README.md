# prometheus-exporter-merger

## Description

An alternative proxy for prometheus exporters.

Provides the feature to merge multiple upstream exporters into a single exporter.

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
