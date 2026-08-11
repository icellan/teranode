package centrifuge_impl

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/require"
)

// TestCheckWebsocketOrigin covers the /connection/websocket origin check:
// same-host and no-Origin requests are always allowed, everything else must
// be explicitly allowlisted. Mirrors
// services/p2p/HandleWebsocket_test.go's TestWebsocketCheckOrigin.
func TestCheckWebsocketOrigin(t *testing.T) {
	tests := []struct {
		name           string
		allowedOrigins []string
		origin         string
		host           string
		want           bool
	}{
		{
			name: "no origin header is allowed",
			host: "node.example.com",
			want: true,
		},
		{
			name:   "same-host origin is allowed",
			origin: "https://node.example.com",
			host:   "node.example.com",
			want:   true,
		},
		{
			name:   "cross-origin request is denied by default",
			origin: "https://evil.example.com",
			host:   "node.example.com",
			want:   false,
		},
		{
			name:           "cross-origin request in the allowlist is allowed",
			allowedOrigins: []string{"https://dashboard.example.com"},
			origin:         "https://dashboard.example.com",
			host:           "node.example.com",
			want:           true,
		},
		{
			name:           "cross-origin request not in the allowlist is denied",
			allowedOrigins: []string{"https://dashboard.example.com"},
			origin:         "https://evil.example.com",
			host:           "node.example.com",
			want:           false,
		},
		{
			name:           "dev-server origin is allowed once allowlisted",
			allowedOrigins: []string{"http://localhost:5173"},
			origin:         "http://localhost:5173",
			host:           "localhost:8090",
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOrigin := checkWebsocketOrigin(tt.allowedOrigins)

			req := httptest.NewRequest(http.MethodGet, "http://"+tt.host+"/connection/websocket", nil)
			req.Host = tt.host

			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			require.Equal(t, tt.want, checkOrigin(req))
		})
	}
}

// TestCentrifuge_wsAllowedOrigins_IncludesDevServerOrigins verifies that the
// dashboard's Vite dev-server origins (settings.Dashboard.DevServerPorts) are
// always permitted, even when asset_wsAllowedOrigins is unset, so `make dev`
// keeps working now that the websocket origin check is default-deny.
func TestCentrifuge_wsAllowedOrigins_IncludesDevServerOrigins(t *testing.T) {
	c := &Centrifuge{
		settings: &settings.Settings{
			Dashboard: settings.DashboardSettings{
				DevServerPorts: []int{5173, 4173},
			},
		},
	}

	origins := c.wsAllowedOrigins()

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8090/connection/websocket", nil)
	req.Host = "localhost:8090"
	req.Header.Set("Origin", "http://localhost:5173")

	require.True(t, checkWebsocketOrigin(origins)(req), "dev-server origin must be allowed so `make dev` keeps working")
}
