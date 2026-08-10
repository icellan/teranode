# Monitoring Teranode

This guide covers the dashboards Teranode ships out of the box, the metrics
that distinguish a healthy node from a degraded one, and how to get started
with alerting. It applies to both the Docker quickstart and Kubernetes
operator deployments — the underlying metrics and dashboards are the same;
only how you reach Grafana/Prometheus differs.

## Dashboards

Teranode ships pre-built Grafana dashboards, provisioned automatically wherever
Prometheus and Grafana are deployed alongside it:

| Dashboard | File | What it shows |
| --- | --- | --- |
| Teranode Service Overview | `deploy/docker/base/grafana_dashboards/teranode/teranode-overview-dashboard.json` | Top-level health across all services: throughput, latencies, error rates |
| Aerospike Batch Index Bottleneck Diagnostic | `deploy/docker/base/grafana_dashboards/teranode/teranode-batch-index-dashboard.json` | Aerospike batch index buffer pressure, a common scaling bottleneck |
| Aerospike Namespace | `deploy/docker/base/grafana_dashboards/aerospike/aerospike-namespace.json` | Namespace-level memory, disk, and object counts |
| Aerospike Latency | `deploy/docker/base/grafana_dashboards/aerospike/aerospike-latency.json` | Read/write/batch latency buckets for the UTXO store |
| BlockAssembler State Monitoring | `compose/grafana/dashboards/blockassembly-state.json` | Block assembler state timeline (Running, Reorging, GetMiningCandidate, ...), state durations, and transition rates — see [dashboard notes](https://github.com/bsv-blockchain/teranode/blob/main/compose/grafana/dashboards/README.md) |

On the Docker quickstart path, these dashboards are provisioned automatically
when the `monitoring` compose profile is enabled (the default). Grafana is
reachable at `http://localhost:3005` and Prometheus at `http://localhost:9090`
(both loopback-only by default). See [Installing with
Docker](docker/minersHowToInstallation.md) and its [Troubleshooting
guide](docker/minersHowToTroubleshooting.md) if Grafana shows no data.

On Kubernetes, Prometheus and Grafana are not deployed by the Teranode
operator itself — point your cluster's existing Prometheus at the Teranode
services' metrics endpoints and import the dashboards above manually
(Grafana → Dashboards → Import).

## Metrics: Healthy vs. Degraded

The full metric catalogue is in the [Prometheus Metrics
Reference](../../references/prometheusMetrics.md). The signals below are the
ones worth watching first — most are visible directly on the Service Overview
dashboard.

### FSM and sync state

- `teranode_blockchain_fsm_current_state` — should sit on `RUNNING` once
  initial sync completes. Stuck on `CATCHINGBLOCKS` or oscillating indicates a
  sync problem; see [Syncing the
  Node](docker/minersHowToSyncTheNode.md).
- `teranode_blockvalidation_catchup_active` and
  `teranode_blockvalidation_processing_blocks_stuck` — non-zero for extended
  periods means the node has fallen behind or a block is wedged.

### Block assembly

- `teranode_blockassembly_current_state` /
  `teranode_blockassembly_state_duration_seconds` — a healthy node spends
  almost all its time in `Running`. Long dwell time in `Reorging` or
  `MovingUp` is worth investigating.
- `teranode_blockassembly_best_block_height` vs. your own chain tip — a gap
  that doesn't close means block assembly is falling behind validation.

### Validation and propagation errors

- `teranode_validator_invalid_transactions` and
  `teranode_propagation_invalid_transactions` — a sustained non-zero rate
  (rather than occasional spikes from normal network traffic) suggests policy
  misconfiguration or an upstream data problem.
- `teranode_aerospike_utxo_errors`, `teranode_aerospike_txmeta_errors`, and
  the SQL-store equivalents (`teranode_sql_utxo_errors`) — any sustained rate
  here points at store-level trouble (disk, memory pressure, connectivity),
  not the chain itself.

### Fork and reorg activity

- `teranode_blockvalidation_fork_count` and
  `teranode_blockvalidation_fork_orphaned_total` — occasional forks are
  normal; a rapidly growing fork count or frequent orphaning suggests network
  or peering issues.

### Cache and store pressure

- `teranode_tx_meta_cache_hits` vs. `teranode_tx_meta_cache_misses` — a
  degrading hit ratio under steady load usually precedes increased UTXO store
  latency.
- `teranode_aerospike_utxo_create_batch` / `_spend_batch` (histograms) —
  rising batch durations are an early indicator of Aerospike contention,
  before it shows up as user-visible slowdown.

## Starter Alerting

Teranode does not ship Prometheus alerting rules — alert thresholds depend on
your hardware, network, and risk tolerance, so this is left to the operator.
As a starting point, consider alerting on:

- `teranode_blockchain_fsm_current_state` not `RUNNING` for more than a few
  minutes after startup completes.
- A sustained non-zero rate of `teranode_validator_invalid_transactions`,
  `teranode_aerospike_utxo_errors`, or `teranode_sql_utxo_errors`.
- `teranode_blockvalidation_processing_blocks_stuck` > 0 for more than a
  couple of minutes.
- `teranode_blockassembly_current_state` stuck outside `Running` beyond the
  block time you'd expect for your network.

The [BlockAssembler State Monitoring dashboard
notes](https://github.com/bsv-blockchain/teranode/blob/main/compose/grafana/dashboards/README.md#alerting-rules)
include worked example Prometheus alert rules (stuck-state, slow mining
candidate generation, reorg frequency) you can adapt as a template rather than
writing rules from scratch.

## Related Documentation

- [Prometheus Metrics Reference](../../references/prometheusMetrics.md)
- [Installing with Docker](docker/minersHowToInstallation.md)
- [Installing with Kubernetes](kubernetes/minersHowToInstallation.md)
- [Troubleshooting (Docker)](docker/minersHowToTroubleshooting.md)
- [Troubleshooting (Kubernetes)](kubernetes/minersHowToTroubleshooting.md)
