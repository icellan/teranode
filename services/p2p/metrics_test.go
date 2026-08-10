package p2p

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestInitPrometheusMetricsIdempotent verifies that calling initPrometheusMetrics
// multiple times (which happens naturally: once via the package init() and again
// from NewServer / other tests in this package) never panics or double-registers
// with the default Prometheus registerer.
func TestInitPrometheusMetricsIdempotent(t *testing.T) {
	require.NotPanics(t, func() {
		initPrometheusMetrics()
		initPrometheusMetrics()
		initPrometheusMetrics()
	})

	require.NotNil(t, prometheusP2PConnectedPeers)
	require.NotNil(t, prometheusP2PBanEvents)
	require.NotNil(t, prometheusP2PCatchupAttempts)
	require.NotNil(t, prometheusP2PCatchupSuccesses)
	require.NotNil(t, prometheusP2PWebsocketConnections)
}

// TestPrometheusMetricsRegisteredWithTeranodeNamespace confirms all p2p metrics
// are registered with the default gatherer under the teranode_p2p_ prefix.
func TestPrometheusMetricsRegisteredWithTeranodeNamespace(t *testing.T) {
	initPrometheusMetrics()

	// CounterVec collectors only surface in Gather() once a label combination
	// has been materialized; touch it (Add(0) is a no-op on the value) so its
	// family is present to check below.
	prometheusP2PBanEvents.WithLabelValues("namespace_check").Add(0)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	checked := map[string]bool{
		"teranode_p2p_connected_peers":         false,
		"teranode_p2p_ban_events_total":        false,
		"teranode_p2p_catchup_attempts_total":  false,
		"teranode_p2p_catchup_successes_total": false,
		"teranode_p2p_websocket_connections":   false,
	}

	for _, family := range families {
		if _, ok := checked[family.GetName()]; ok {
			checked[family.GetName()] = true
		}
	}

	for name, found := range checked {
		require.True(t, found, "metric %s should be registered", name)
	}
}

// TestPrometheusMetricsIncrementAndSet exercises the increment/set call sites
// used by the actual wiring (ban events, catchup attempts/successes, websocket
// connection gauge, connected-peer gauge) to confirm they behave as plain
// counters/gauges.
func TestPrometheusMetricsIncrementAndSet(t *testing.T) {
	initPrometheusMetrics()

	attemptsBefore := testutil.ToFloat64(prometheusP2PCatchupAttempts)
	successesBefore := testutil.ToFloat64(prometheusP2PCatchupSuccesses)
	banBefore := testutil.ToFloat64(prometheusP2PBanEvents.WithLabelValues("test_reason"))

	prometheusP2PCatchupAttempts.Inc()
	prometheusP2PCatchupSuccesses.Inc()
	prometheusP2PBanEvents.WithLabelValues("test_reason").Inc()
	prometheusP2PConnectedPeers.Set(3)
	prometheusP2PWebsocketConnections.Set(2)

	require.Equal(t, attemptsBefore+1, testutil.ToFloat64(prometheusP2PCatchupAttempts))
	require.Equal(t, successesBefore+1, testutil.ToFloat64(prometheusP2PCatchupSuccesses))
	require.Equal(t, banBefore+1, testutil.ToFloat64(prometheusP2PBanEvents.WithLabelValues("test_reason")))
	require.InDelta(t, 3, testutil.ToFloat64(prometheusP2PConnectedPeers), 0.0001)
	require.InDelta(t, 2, testutil.ToFloat64(prometheusP2PWebsocketConnections), 0.0001)
}

// TestClientChannelMapUpdatesWebsocketConnectionsGauge confirms add/remove on
// the websocket client registry keeps the connection-count gauge in sync,
// covering the actual increment/decrement call sites wired into HandleWebSocket.
func TestClientChannelMapUpdatesWebsocketConnectionsGauge(t *testing.T) {
	initPrometheusMetrics()

	cm := newClientChannelMap()

	ch1 := make(chan []byte, 1)
	ch2 := make(chan []byte, 1)

	cm.add(ch1)
	require.InDelta(t, 1, testutil.ToFloat64(prometheusP2PWebsocketConnections), 0.0001)

	cm.add(ch2)
	require.InDelta(t, 2, testutil.ToFloat64(prometheusP2PWebsocketConnections), 0.0001)

	cm.remove(ch1)
	require.InDelta(t, 1, testutil.ToFloat64(prometheusP2PWebsocketConnections), 0.0001)

	cm.remove(ch2)
	require.InDelta(t, 0, testutil.ToFloat64(prometheusP2PWebsocketConnections), 0.0001)
}
