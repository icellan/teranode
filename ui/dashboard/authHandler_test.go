package dashboard

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestAuthHandler() *AuthHandler {
	testSettings := &settings.Settings{
		Dashboard: settings.DashboardSettings{
			DevServerPorts: []int{5173, 4173},
			WebSocketPort:  "8090",
			WebSocketPath:  "/connection/websocket",
		},
	}

	return NewAuthHandler(ulogger.TestLogger{}, testSettings)
}

func TestWebSocketConfigHandler(t *testing.T) {
	tests := []struct {
		name         string
		host         string
		scheme       string
		tls          bool
		expectedURL  string
		expectedPort string
	}{
		{
			name:         "Development server Vite port 5173",
			host:         "localhost:5173",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://localhost:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "Development server Vite port 4173",
			host:         "localhost:4173",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://localhost:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "Production HTTP",
			host:         "localhost:8090",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://localhost:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "Production HTTPS",
			host:         "localhost:8090",
			scheme:       "https",
			tls:          true,
			expectedURL:  "wss://localhost:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "Docker environment",
			host:         "teranode1:8090",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://teranode1:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "IPv4 with port",
			host:         "192.168.1.100:8090",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://192.168.1.100:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "IPv6 with port",
			host:         "[2406:da18:1f7:353a:b079:da22:c7d5:e166]:8090",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://2406:da18:1f7:353a:b079:da22:c7d5:e166:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "IPv6 with port HTTPS",
			host:         "[2406:da18:1f7:353a:b079:da22:c7d5:e166]:8090",
			scheme:       "https",
			tls:          true,
			expectedURL:  "wss://2406:da18:1f7:353a:b079:da22:c7d5:e166:8090/connection/websocket",
			expectedPort: "8090",
		},
		{
			name:         "Standard HTTP port (no explicit port)",
			host:         "example.com",
			scheme:       "http",
			tls:          false,
			expectedURL:  "ws://example.com/connection/websocket",
			expectedPort: "",
		},
		{
			name:         "Standard HTTPS port (no explicit port)",
			host:         "example.com",
			scheme:       "https",
			tls:          true,
			expectedURL:  "wss://example.com/connection/websocket",
			expectedPort: "",
		},
		{
			name:         "Custom domain with port",
			host:         "dashboard.mycompany.com:9000",
			scheme:       "https",
			tls:          true,
			expectedURL:  "wss://dashboard.mycompany.com:9000/connection/websocket",
			expectedPort: "9000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authHandler := createTestAuthHandler()

			// Create Echo context
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/config/websocket", nil)
			req.Host = tt.host

			// Set scheme and TLS
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}

			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			// Mock the scheme
			if tt.scheme == "https" {
				c.Set("scheme", "https")
			}

			// Call the handler
			err := authHandler.WebSocketConfigHandler(c)
			require.NoError(t, err)

			// Check response
			assert.Equal(t, http.StatusOK, rec.Code)

			var response map[string]interface{}
			err = json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			websocketURL, exists := response["websocketUrl"]
			require.True(t, exists, "websocketUrl should be present in response")

			assert.Equal(t, tt.expectedURL, websocketURL,
				"WebSocket URL should match expected for host: %s", tt.host)
		})
	}
}

func TestWebSocketConfigHandler_ConfigurablePorts(t *testing.T) {
	// Test with different configuration
	testSettings := &settings.Settings{
		Dashboard: settings.DashboardSettings{
			DevServerPorts: []int{3000, 3001}, // Different dev ports
			WebSocketPort:  "9090",            // Different WebSocket port
			WebSocketPath:  "/ws",             // Different path
		},
	}

	authHandler := NewAuthHandler(ulogger.TestLogger{}, testSettings)

	tests := []struct {
		name        string
		host        string
		expectedURL string
	}{
		{
			name:        "Custom dev port 3000",
			host:        "localhost:3000",
			expectedURL: "ws://localhost:9090/ws",
		},
		{
			name:        "Custom dev port 3001",
			host:        "localhost:3001",
			expectedURL: "ws://localhost:9090/ws",
		},
		{
			name:        "Production uses current port",
			host:        "localhost:8080",
			expectedURL: "ws://localhost:8080/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/config/websocket", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := authHandler.WebSocketConfigHandler(c)
			require.NoError(t, err)

			assert.Equal(t, http.StatusOK, rec.Code)

			var response map[string]interface{}
			err = json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			websocketURL := response["websocketUrl"]
			assert.Equal(t, tt.expectedURL, websocketURL)
		})
	}
}

func TestWebSocketConfigHandler_EdgeCases(t *testing.T) {
	authHandler := createTestAuthHandler()

	tests := []struct {
		name        string
		host        string
		expectedURL string
		description string
	}{
		{
			name:        "IPv6 localhost",
			host:        "[::1]:5173",
			expectedURL: "ws://::1:5173/connection/websocket",
			description: "IPv6 localhost uses current port (not treated as dev server)",
		},
		{
			name:        "Malformed host handled gracefully",
			host:        "invalid::host::format",
			expectedURL: "ws://invalid::host::format/connection/websocket",
			description: "Invalid host should be passed through (net.SplitHostPort will fail gracefully)",
		},
		{
			name:        "Empty host",
			host:        "",
			expectedURL: "ws:///connection/websocket",
			description: "Empty host should be handled",
		},
		{
			name:        "Host with no port",
			host:        "myserver",
			expectedURL: "ws://myserver/connection/websocket",
			description: "Host without port should work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/config/websocket", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := authHandler.WebSocketConfigHandler(c)
			require.NoError(t, err, tt.description)

			assert.Equal(t, http.StatusOK, rec.Code)

			var response map[string]interface{}
			err = json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			websocketURL := response["websocketUrl"]
			assert.Equal(t, tt.expectedURL, websocketURL, tt.description)
		})
	}
}

func TestWebSocketConfigHandler_ProtocolDetection(t *testing.T) {
	authHandler := createTestAuthHandler()

	tests := []struct {
		name          string
		host          string
		hasTLS        bool
		scheme        string
		expectedProto string
	}{
		{
			name:          "HTTP without TLS",
			host:          "localhost:8090",
			hasTLS:        false,
			scheme:        "http",
			expectedProto: "ws",
		},
		{
			name:          "HTTPS with TLS",
			host:          "localhost:8090",
			hasTLS:        true,
			scheme:        "https",
			expectedProto: "wss",
		},
		{
			name:          "TLS detected from connection state",
			host:          "localhost:8090",
			hasTLS:        true,
			scheme:        "http", // Even if scheme is http, TLS connection should use wss
			expectedProto: "wss",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/config/websocket", nil)
			req.Host = tt.host

			if tt.hasTLS {
				req.TLS = &tls.ConnectionState{}
			}

			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := authHandler.WebSocketConfigHandler(c)
			require.NoError(t, err)

			assert.Equal(t, http.StatusOK, rec.Code)

			var response map[string]interface{}
			err = json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			websocketURL := response["websocketUrl"].(string)
			assert.True(t, strings.HasPrefix(websocketURL, tt.expectedProto+"://"),
				"Expected protocol %s, got URL: %s", tt.expectedProto, websocketURL)
		})
	}
}

// captureLogger records every formatted log line so tests can assert on their content.
type captureLogger struct {
	ulogger.TestLogger

	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) record(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *captureLogger) Debugf(format string, args ...interface{}) { l.record(format, args...) }
func (l *captureLogger) Infof(format string, args ...interface{})  { l.record(format, args...) }
func (l *captureLogger) Warnf(format string, args ...interface{})  { l.record(format, args...) }
func (l *captureLogger) Errorf(format string, args ...interface{}) { l.record(format, args...) }

func (l *captureLogger) output() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.lines, "\n")
}

func newCredentialAuthHandler(logger ulogger.Logger, user, pass string, requireCredentials, secureCookies bool) *AuthHandler {
	testSettings := &settings.Settings{
		Asset: settings.AssetSettings{
			RequireAuthCredentials: requireCredentials,
			SecureCookies:          secureCookies,
		},
		RPC: settings.RPCSettings{
			RPCUser: user,
			RPCPass: pass,
		},
	}

	return NewAuthHandler(logger, testSettings)
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestCheckAuth_MissingCredentials(t *testing.T) {
	tests := []struct {
		name               string
		user               string
		pass               string
		requireCredentials bool
		expected           bool
	}{
		{name: "both empty, warn-only", user: "", pass: "", requireCredentials: false, expected: true},
		{name: "empty user, warn-only", user: "", pass: "secret", requireCredentials: false, expected: true},
		{name: "empty pass, warn-only", user: "admin", pass: "", requireCredentials: false, expected: true},
		{name: "both empty, fail closed", user: "", pass: "", requireCredentials: true, expected: false},
		{name: "empty user, fail closed", user: "", pass: "secret", requireCredentials: true, expected: false},
		{name: "empty pass, fail closed", user: "admin", pass: "", requireCredentials: true, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCredentialAuthHandler(ulogger.TestLogger{}, tt.user, tt.pass, tt.requireCredentials, false)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/fsm/state", nil)
			require.Equal(t, tt.expected, h.CheckAuth(req))
		})
	}
}

func TestCheckAuth_ConfiguredCredentials(t *testing.T) {
	h := newCredentialAuthHandler(ulogger.TestLogger{}, "admin", "secret", false, false)

	t.Run("no credentials supplied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fsm/state", nil)
		require.False(t, h.CheckAuth(req))
	})

	t.Run("wrong credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fsm/state", nil)
		req.Header.Set("Authorization", basicHeader("admin", "wrong"))
		require.False(t, h.CheckAuth(req))
	})

	t.Run("correct credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fsm/state", nil)
		req.Header.Set("Authorization", basicHeader("admin", "secret"))
		require.True(t, h.CheckAuth(req))
	})

	t.Run("correct credentials in cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fsm/state", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: basicHeader("admin", "secret")})
		require.True(t, h.CheckAuth(req))
	})
}

func TestCheckAuthHandler_ShortAuthorizationHeader(t *testing.T) {
	// Headers shorter than ten bytes must not panic the request.
	for _, header := range []string{"x", "Basic", "Basic ab"} {
		t.Run(header, func(t *testing.T) {
			h := newCredentialAuthHandler(ulogger.TestLogger{}, "admin", "secret", false, false)

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()

			require.NotPanics(t, func() {
				require.NoError(t, h.CheckAuthHandler(e.NewContext(req, rec)))
			})
			require.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestCheckAuthHandler_DoesNotLogCredentialMaterial(t *testing.T) {
	logger := &captureLogger{}
	h := newCredentialAuthHandler(logger, "admin", "sup3rsecret", false, false)

	authHeader := basicHeader("admin", "sup3rsecret")

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	req.Header.Set("Authorization", authHeader)
	rec := httptest.NewRecorder()

	require.NoError(t, h.CheckAuthHandler(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	logged := logger.output()
	encoded := strings.TrimPrefix(authHeader, "Basic ")

	require.NotContains(t, logged, "sup3rsecret")
	require.NotContains(t, logged, encoded)
	// Not even a prefix of the encoded credential may be logged.
	require.NotContains(t, logged, encoded[:4])
}

func TestLoginHandler_SecureCookie(t *testing.T) {
	tests := []struct {
		name          string
		secureCookies bool
	}{
		{name: "secure cookies disabled", secureCookies: false},
		{name: "secure cookies enabled", secureCookies: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCredentialAuthHandler(ulogger.TestLogger{}, "admin", "secret", false, tt.secureCookies)

			e := echo.New()

			t.Run("form login", func(t *testing.T) {
				form := url.Values{"username": {"admin"}, "password": {"secret"}}
				req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(form.Encode()))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
				rec := httptest.NewRecorder()

				require.NoError(t, h.LoginHandler(e.NewContext(req, rec)))
				require.Equal(t, http.StatusOK, rec.Code)
				requireAuthCookieSecure(t, rec, tt.secureCookies)
			})

			t.Run("header login", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				req.Header.Set("Authorization", basicHeader("admin", "secret"))
				rec := httptest.NewRecorder()

				require.NoError(t, h.LoginHandler(e.NewContext(req, rec)))
				require.Equal(t, http.StatusOK, rec.Code)
				requireAuthCookieSecure(t, rec, tt.secureCookies)
			})

			t.Run("logout", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
				rec := httptest.NewRecorder()

				require.NoError(t, h.LogoutHandler(e.NewContext(req, rec)))
				require.Equal(t, http.StatusOK, rec.Code)
				requireAuthCookieSecure(t, rec, tt.secureCookies)
			})
		})
	}
}

func requireAuthCookieSecure(t *testing.T, rec *httptest.ResponseRecorder, expected bool) {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == cookieName {
			require.Equal(t, expected, cookie.Secure)
			return
		}
	}

	t.Fatalf("no %s cookie was set", cookieName)
}
