package util

import "fmt"

// DevServerOrigins returns the http/https localhost Origin header values for
// the given Vite dev-server ports (settings.Dashboard.DevServerPorts). It is
// used to auto-allow the dashboard's dev server against websocket endpoints
// that enforce an origin allowlist (services/p2p/HandleWebsocket.go,
// services/asset/centrifuge_impl/centrifuge.go), so `make dev` keeps working
// without requiring an explicit p2p_wsAllowedOrigins/asset_wsAllowedOrigins
// override.
func DevServerOrigins(ports []int) []string {
	origins := make([]string, 0, len(ports)*2)

	for _, port := range ports {
		origins = append(origins,
			fmt.Sprintf("http://localhost:%d", port),
			fmt.Sprintf("https://localhost:%d", port),
		)
	}

	return origins
}
