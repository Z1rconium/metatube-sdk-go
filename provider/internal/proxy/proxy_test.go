package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/metatube-community/metatube-sdk-go/provider/internal/vpngate"
)

func TestNew(t *testing.T) {
	_, err := New("")
	assert.Error(t, err)
	_, err = New("127.0.0.1:7928")
	assert.Error(t, err)
	_, err = New("ftp://127.0.0.1:7928")
	assert.Error(t, err)

	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7928", m.Endpoint())
	assert.False(t, m.InFallback())

	m, err = New("socks5h://127.0.0.1:7928")
	require.NoError(t, err)
	assert.Equal(t, "socks5h://127.0.0.1:7928", m.Endpoint())
}

func TestManager_ProxyFunc(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)

	u, err := m.ProxyFunc(&http.Request{})
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "127.0.0.1:7928", u.Host)

	// Fall back to direct server egress.
	m.Fallback()
	assert.True(t, m.InFallback())
	u, err = m.ProxyFunc(&http.Request{})
	require.NoError(t, err)
	assert.Nil(t, u)

	// Restore the proxy.
	m.Restore()
	assert.False(t, m.InFallback())
	u, err = m.ProxyFunc(&http.Request{})
	require.NoError(t, err)
	require.NotNil(t, u)
}

func TestManager_CanRotate(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)

	// No gateway configured.
	assert.False(t, m.CanRotate())

	gateway, err := vpngate.NewClient(vpngate.Config{
		URL:      "http://admin:secret@127.0.0.1:8787/secret",
		Country:  "JP",
		Username: "admin",
		Password: "secret",
	})
	require.NoError(t, err)
	m.SetGateway(gateway, "jp")
	assert.Equal(t, "JP", m.Country())
	assert.True(t, m.CanRotate())

	// The proxy is being probed again only after the cooldown.
	m.Fallback()
	assert.False(t, m.CanRotate())
	m.cooldown = 0
	assert.True(t, m.CanRotate())
}

func TestManager_Rotate(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)

	// No gateway configured.
	assert.ErrorIs(t, m.Rotate(context.Background()), ErrNoGateway)
}

// newBusyGateway returns a fake gateway that rejects node switch requests, as
// it does while reconnecting by itself. The gateway is reported as connecting
// until the given number of status requests have been served.
func newBusyGateway(t *testing.T, connectingPolls int32) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/secret/api/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "token", Path: "/secret/"})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/secret/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		connecting := polls.Add(1) <= connectingPolls
		_, _ = fmt.Fprintf(w, `{"nodes":[
			{"id":"JP_1.2.3.4_443_udp","country_short":"JP","probe_status":"available","latency_ms":120,"active":%t},
			{"id":"JP_5.6.7.8_443_tcp","country_short":"JP","probe_status":"available","latency_ms":80,"active":false}
		],"state":{"proxy_ok":%t,"is_connecting":%t}}`, !connecting, !connecting, connecting)
	})
	mux.HandleFunc("/secret/api/connect", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"error":"switch task is running"}`))
	})
	return httptest.NewServer(mux)
}

func TestManager_Rotate_GatewayBusy(t *testing.T) {
	newManager := func(t *testing.T, srv *httptest.Server) *Manager {
		t.Helper()
		gateway, err := vpngate.NewClient(vpngate.Config{URL: srv.URL + "/secret", Username: "admin", Password: "secret"})
		require.NoError(t, err)
		m, err := New("http://127.0.0.1:7928")
		require.NoError(t, err)
		m.SetGateway(gateway, "JP")
		m.recoverPoll = 10 * time.Millisecond
		m.recoverTimeout = 500 * time.Millisecond
		return m
	}

	t.Run("recovers", func(t *testing.T) {
		srv := newBusyGateway(t, 3)
		defer srv.Close()
		m := newManager(t, srv)
		m.Fallback()
		// The gateway settles on a JP node by itself, so no error is returned
		// and the proxy is resumed.
		require.NoError(t, m.Rotate(context.Background()))
		assert.False(t, m.InFallback())
	})

	t.Run("timeout", func(t *testing.T) {
		srv := newBusyGateway(t, 1<<30)
		defer srv.Close()
		m := newManager(t, srv)
		m.Fallback()
		assert.Error(t, m.Rotate(context.Background()))
		assert.True(t, m.InFallback())
	})
}

func TestIsTransportError(t *testing.T) {
	assert.False(t, IsTransportError(nil))
	assert.False(t, IsTransportError(errors.New("content-id-not-found")))
	assert.False(t, IsTransportError(&url.Error{Op: "redirect", URL: "https://example.com", Err: errors.New("require-new-handler")}))

	// Dial failures.
	assert.True(t, IsTransportError(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}))
	assert.True(t, IsTransportError(&url.Error{Op: "Get", URL: "https://example.com", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}))
	assert.True(t, IsTransportError(os.ErrDeadlineExceeded))
	assert.True(t, IsTransportError(context.DeadlineExceeded))

	// Gateway errors returned by the VPN gateway proxy.
	assert.True(t, IsTransportError(errors.New(http.StatusText(http.StatusBadGateway))))
	assert.True(t, IsTransportError(errors.New(http.StatusText(http.StatusServiceUnavailable))))
	assert.True(t, IsTransportError(errors.New(http.StatusText(http.StatusGatewayTimeout))))

	// Timeouts.
	var netErr net.Error = &net.DNSError{Err: "timeout", IsTimeout: true}
	assert.True(t, IsTransportError(netErr))

	assert.False(t, IsTransportError(errors.New("not found")))
}

func TestManager_Transport(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	require.NotNil(t, m.Transport())
	assert.NotNil(t, m.Transport().Proxy)

	// The transport must not outlive the test.
	t.Cleanup(func() { m.Transport().CloseIdleConnections() })
	time.Sleep(time.Millisecond)
}

func TestCheckStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/bad-gateway":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	client := &http.Client{Transport: CheckStatus(http.DefaultTransport)}

	resp, err := client.Get(srv.URL + "/ok")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// A gateway error is surfaced as an error.
	_, err = client.Get(srv.URL + "/bad-gateway")
	require.Error(t, err)
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadGateway, statusErr.Code)
	assert.True(t, IsTransportError(err))

	// Any other unexpected status is surfaced as well, but it is not treated
	// as an unusable exit IP.
	_, err = client.Get(srv.URL + "/internal-server-error")
	require.Error(t, err)
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusInternalServerError, statusErr.Code)
	assert.False(t, IsTransportError(err))
}
