package fanza

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/metatube-community/metatube-sdk-go/internal/envconfig"
)

// newTestVPNGateway returns a fake aimili-vpngate management API.
func newTestVPNGateway(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/secret/api/login", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		if payload["username"] != "admin" || payload["password"] != "secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "token", Path: "/secret/"})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/secret/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[
			{"id":"JP_1.2.3.4_443_udp","ip":"1.2.3.4","country_short":"JP","probe_status":"available","latency_ms":100,"active":false}
		],"state":{"proxy_ok":true}}`))
	})
	mux.HandleFunc("/secret/api/connect", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"message":"Connected"}`))
	})
	return httptest.NewServer(mux)
}

func TestFANZA_fetchWithExitFallback(t *testing.T) {
	t.Run("NoProxy", func(t *testing.T) {
		// Without a proxy there is nothing to fall back from.
		fz := New()
		var calls int
		err := fz.fetchWithExitFallback(func() error {
			calls++
			return ErrRegionNotAvailable
		})
		require.ErrorIs(t, err, ErrRegionNotAvailable)
		assert.Equal(t, 1, calls)
	})

	t.Run("ProxyWithoutGateway", func(t *testing.T) {
		// A proxy that the user configured by hand is used as is.
		fz := New()
		require.NoError(t, fz.SetProxy("http://127.0.0.1:7928"))
		var calls int
		err := fz.fetchWithExitFallback(func() error {
			calls++
			return ErrRegionNotAvailable
		})
		require.ErrorIs(t, err, ErrRegionNotAvailable)
		assert.Equal(t, 1, calls)
		assert.False(t, fz.proxy.InFallback())
	})

	t.Run("RotateExitIP", func(t *testing.T) {
		gateway := newTestVPNGateway(t)
		defer gateway.Close()

		fz := New()
		require.NoError(t, fz.SetProxy("http://127.0.0.1:7928"))
		config := envconfig.NewConfig()
		config.Set("vpngate_url", gateway.URL+"/secret")
		config.Set("vpngate_username", "admin")
		config.Set("vpngate_password", "secret")
		require.NoError(t, fz.SetConfig(config))

		// A new exit IP is acquired and the request is retried successfully.
		var calls int
		err := fz.fetchWithExitFallback(func() error {
			calls++
			if calls == 1 {
				return ErrRegionNotAvailable
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
		assert.False(t, fz.proxy.InFallback())
	})

	t.Run("RotateExitIPFailed", func(t *testing.T) {
		gateway := newTestVPNGateway(t)
		defer gateway.Close()

		fz := New()
		require.NoError(t, fz.SetProxy("http://127.0.0.1:7928"))
		config := envconfig.NewConfig()
		config.Set("vpngate_url", gateway.URL+"/secret")
		config.Set("vpngate_username", "admin")
		config.Set("vpngate_password", "secret")
		require.NoError(t, fz.SetConfig(config))

		// The new exit IP does not help either, fall back to direct egress.
		var calls int
		err := fz.fetchWithExitFallback(func() error {
			calls++
			return ErrRegionNotAvailable
		})
		require.ErrorIs(t, err, ErrRegionNotAvailable)
		assert.Equal(t, 3, calls)
		assert.True(t, fz.proxy.InFallback())
	})
}

func TestFANZA_SetConfig(t *testing.T) {
	gateway := newTestVPNGateway(t)
	defer gateway.Close()

	fz := New()
	config := envconfig.NewConfig()
	config.Set("vpngate_url", gateway.URL+"/secret")
	config.Set("vpngate_username", "admin")
	config.Set("vpngate_password", "secret")
	config.Set("vpngate_country", "jp")
	require.NoError(t, fz.SetConfig(config))

	// The default gateway proxy is enabled when no proxy is configured.
	require.NotNil(t, fz.proxy)
	assert.Equal(t, "http://127.0.0.1:7928", fz.proxy.Endpoint())
	assert.Equal(t, "JP", fz.proxy.Country())
}
