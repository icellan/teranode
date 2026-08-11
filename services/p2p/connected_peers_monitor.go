package p2p

import (
	"context"
	"time"
)

// connectedPeersPollInterval controls how often prometheusP2PConnectedPeers is
// refreshed from the live peer set.
const connectedPeersPollInterval = 5 * time.Second

// startConnectedPeersMonitor runs for the lifetime of ctx, keeping
// prometheusP2PConnectedPeers in sync with the live peer set on its own
// ticker. It is intentionally independent of the NAT-diagnostics logging
// goroutine started elsewhere in Start(): tying the gauge update to that
// goroutine's ticker meant the metric went stale between its (much longer)
// ticks and would stop updating entirely if that goroutine ever exited or
// was refactored away. The returned channel is closed once the monitor
// goroutine has exited, so callers (mainly tests) can synchronize on
// shutdown.
func (s *Server) startConnectedPeersMonitor(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		s.updateConnectedPeersGauge()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.updateConnectedPeersGauge()
			}
		}
	}()

	return done
}

// updateConnectedPeersGauge sets prometheusP2PConnectedPeers to the current
// number of peers reported by the P2P client. A nil client (e.g. before
// NewServer has finished wiring the service) is a no-op.
func (s *Server) updateConnectedPeersGauge() {
	if s.P2PClient == nil {
		return
	}
	prometheusP2PConnectedPeers.Set(float64(len(s.P2PClient.GetPeers())))
}
