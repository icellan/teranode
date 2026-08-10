# Grafana Dashboards

Dashboards in this directory are auto-provisioned into Grafana via `main.yaml`.

## Teranode Overview

**File:** `teranode/teranode-overview-dashboard.json`

The primary day-to-day operational dashboard (~26 panels), covering the main
Teranode services and pipeline stages. Start here for general health checks.

## Other dashboards in this stack

| Dashboard | File | Scope |
|-----------|------|-------|
| Dispatcher Batch Queue | `teranode/teranode-batch-index-dashboard.json` | Batch/queue internals |
| Aerospike Latency | `aerospike/aerospike-latency.json` | UTXO-store dependency latency |
| Aerospike Namespace | `aerospike/aerospike-namespace.json` | UTXO-store dependency namespace stats |

## Not shipped here

The `compose` stack (`compose/grafana/dashboards/`) additionally ships a
**BlockAssembler State Timeline** dashboard (`blockassembly-state.json`,
documented in that directory's own README) that is not present in this
docker-base stack. Conversely, this stack's Teranode Overview and Aerospike
dashboards are not present under `compose/grafana/dashboards/`. If you run one
stack and want a dashboard from the other, copy the relevant JSON file across
manually — the panels reference standard Prometheus metrics and are not tied
to a specific deployment.
