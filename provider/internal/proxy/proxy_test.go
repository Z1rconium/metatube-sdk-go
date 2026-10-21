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
	"sync"
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

	// Fall back to the default server egress.
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

// fakeGateway is a fake aimili-vpngate management API.
type fakeGateway struct {
	*httptest.Server

	// connectingPolls is the number of the status requests that report the
	// gateway as switching nodes by itself, in which case switch requests are
	// rejected.
	connectingPolls int32
	// failConnects is the number of the switch requests that fail on
	// pre-check, before one succeeds.
	failConnects int32
	// conflicts is the number of the switch requests that are rejected
	// because the gateway is switching nodes by itself.
	conflicts int32

	polls    atomic.Int32
	connects atomic.Int32
}

func newFakeGateway(t *testing.T, connectingPolls, failConnects int32) *fakeGateway {
	t.Helper()
	g := &fakeGateway{connectingPolls: connectingPolls, failConnects: failConnects}
	mux := http.NewServeMux()
	mux.HandleFunc("/secret/api/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "token", Path: "/secret/"})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/secret/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		g.polls.Add(1)
		connecting := g.connecting()
		_, _ = fmt.Fprintf(w, `{"nodes":[
			{"id":"JP_1.2.3.4_443_udp","country_short":"JP","probe_status":"available","latency_ms":120,"active":%t},
			{"id":"JP_5.6.7.8_443_tcp","country_short":"JP","probe_status":"available","latency_ms":80,"active":false},
			{"id":"JP_9.9.9.9_443_tcp","country_short":"JP","probe_status":"available","latency_ms":90,"active":false},
			{"id":"JP_8.8.8.8_443_tcp","country_short":"JP","probe_status":"available","latency_ms":100,"active":false},
			{"id":"JP_7.7.7.7_443_tcp","country_short":"JP","probe_status":"available","latency_ms":110,"active":false},
			{"id":"US_6.6.6.6_443_tcp","country_short":"US","probe_status":"available","latency_ms":1,"active":false}
		],"state":{"proxy_ok":%t,"is_connecting":%t}}`, !connecting, !connecting, connecting)
	})
	mux.HandleFunc("/secret/api/connect", func(w http.ResponseWriter, r *http.Request) {
		n := g.connects.Add(1)
		switch {
		case g.connecting():
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error":"switch task is running"}`))
		case n <= g.conflicts:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error":"switch task is running"}`))
		case n <= g.conflicts+g.failConnects:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"pre-check failed"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// connecting reports whether the gateway is switching nodes by itself. It is
// derived from the number of the status requests served so far, so that the
// gateway "recovers" as it is being polled.
func (g *fakeGateway) connecting() bool {
	return g.polls.Load() <= g.connectingPolls
}

func newTestManager(t *testing.T, g *fakeGateway) *Manager {
	t.Helper()
	gateway, err := vpngate.NewClient(vpngate.Config{URL: g.URL + "/secret", Username: "admin", Password: "secret"})
	require.NoError(t, err)
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	m.SetGateway(gateway, "JP")
	m.recoverPoll = 10 * time.Millisecond
	m.recoverGrace = 30 * time.Millisecond
	m.recoverTimeout = 500 * time.Millisecond
	return m
}

func TestManager_Rotate(t *testing.T) {
	t.Run("SelfHealing", func(t *testing.T) {
		// The gateway recovers by itself, so no switch request is sent.
		g := newFakeGateway(t, 3, 0)
		m := newTestManager(t, g)
		m.Fallback()
		require.NoError(t, m.Rotate(context.Background()))
		assert.EqualValues(t, 0, g.connects.Load())
		assert.False(t, m.InFallback())
	})

	t.Run("SelfHealingTimeout", func(t *testing.T) {
		g := newFakeGateway(t, 1<<30, 0)
		m := newTestManager(t, g)
		m.Fallback()
		assert.Error(t, m.Rotate(context.Background()))
		assert.True(t, m.InFallback())
		assert.EqualValues(t, 0, m.Generation())
	})

	t.Run("NextCandidate", func(t *testing.T) {
		// The pre-check of the best node fails, so the next one is tried.
		g := newFakeGateway(t, 0, 1)
		m := newTestManager(t, g)
		require.NoError(t, m.Rotate(context.Background()))
		assert.EqualValues(t, 2, g.connects.Load())
		assert.EqualValues(t, 1, m.Generation())
	})

	t.Run("AllCandidatesFailed", func(t *testing.T) {
		g := newFakeGateway(t, 0, 100)
		m := newTestManager(t, g)
		assert.Error(t, m.Rotate(context.Background()))
		assert.EqualValues(t, maxSwitchCandidates, g.connects.Load())
		assert.EqualValues(t, 0, m.Generation())
	})

	t.Run("SwitchConflict", func(t *testing.T) {
		// The gateway is switching nodes by itself and rejects our request,
		// so we wait for it to settle instead of trying another node.
		g := newFakeGateway(t, 0, 0)
		g.conflicts = 1
		m := newTestManager(t, g)
		require.NoError(t, m.Rotate(context.Background()))
		assert.EqualValues(t, 1, g.connects.Load()) // no next candidate.
		assert.EqualValues(t, 1, m.Generation())
	})

	t.Run("StaleGeneration", func(t *testing.T) {
		// The exit IP has been rotated since the caller observed its failure.
		g := newFakeGateway(t, 0, 0)
		m := newTestManager(t, g)
		require.NoError(t, m.Rotate(context.Background()))
		require.NoError(t, m.RotateFrom(context.Background(), 0))
		assert.EqualValues(t, 1, g.connects.Load())
		assert.EqualValues(t, 1, m.Generation())
	})

	t.Run("Concurrent", func(t *testing.T) {
		// A burst of the failures of the same exit IP rotates it only once.
		g := newFakeGateway(t, 0, 0)
		m := newTestManager(t, g)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				assert.NoError(t, m.RotateFrom(context.Background(), 0))
			}()
		}
		wg.Wait()
		assert.EqualValues(t, 1, g.connects.Load())
		assert.EqualValues(t, 1, m.Generation())
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		g := newFakeGateway(t, 0, 0)
		m := newTestManager(t, g)
		m.rotateSem <- struct{}{} // a rotation is in flight.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.ErrorIs(t, m.Rotate(ctx), context.Canceled)
	})
}

func TestManager_Rotate_Budget(t *testing.T) {
	// A gateway that accepts the connections but never answers must not hold
	// the rotation (and the semaphore) longer than the budget.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret/api/login":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/secret/api/nodes":
			_, _ = w.Write([]byte(`{"nodes":[
				{"id":"JP_1.1.1.1_443_tcp","country_short":"JP","probe_status":"available","latency_ms":10,"active":false}
			],"state":{"proxy_ok":true}}`))
		case "/secret/api/connect":
			<-release // never answers, until the client gives up.
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	gateway, err := vpngate.NewClient(vpngate.Config{
		URL: srv.URL + "/secret", Username: "admin", Password: "secret",
	})
	require.NoError(t, err)
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	m.SetGateway(gateway, "JP")
	m.recoverPoll = 10 * time.Millisecond
	m.recoverGrace = 10 * time.Millisecond
	m.rotateTimeout = 150 * time.Millisecond

	start := time.Now()
	err = m.Rotate(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Empty(t, m.rotateSem) // the budget does not leak the semaphore.
	assert.EqualValues(t, 0, m.Generation())
}

func TestCountryMismatch(t *testing.T) {
	assert.Empty(t, countryMismatch(vpngate.Status{}, "JP"))
	assert.Empty(t, countryMismatch(vpngate.Status{
		RoutingMode: routingModeFixedRegion, ForceCountry: "JP",
	}, "jp"))
	assert.NotEmpty(t, countryMismatch(vpngate.Status{RoutingMode: "auto"}, "JP"))
	assert.NotEmpty(t, countryMismatch(vpngate.Status{ForceCountry: "US"}, "JP"))
}

func TestManager_ProxyFunc_FallbackProxy(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)

	// While falling back, the proxy of the user is used instead of the exit
	// of the gateway, so that the traffic never goes direct if the user has
	// configured a proxy for the server.
	userProxy, _ := url.Parse("http://127.0.0.1:10800")
	m.fallbackProxy = func(*http.Request) (*url.URL, error) { return userProxy, nil }
	m.Fallback()
	req := httptest.NewRequest(http.MethodGet, "https://www.dmm.co.jp/", nil)
	u, err := m.ProxyFunc(req)
	require.NoError(t, err)
	assert.Equal(t, userProxy, u)

	m.Restore()
	u, err = m.ProxyFunc(req)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7928", u.String())
}

func TestManager_ProxyFunc_ResumeAfterCooldown(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	m.cooldown = 50 * time.Millisecond

	u, err := m.ProxyFunc(nil)
	require.NoError(t, err)
	require.NotNil(t, u)

	// Direct egress within the cooldown.
	m.Fallback()
	u, err = m.ProxyFunc(nil)
	require.NoError(t, err)
	assert.Nil(t, u)
	assert.True(t, m.InFallback())

	// The proxy is probed again once the cooldown has elapsed.
	time.Sleep(60 * time.Millisecond)
	u, err = m.ProxyFunc(nil)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.False(t, m.InFallback())
}

func TestManager_Rotate_NoGateway(t *testing.T) {
	m, err := New("http://127.0.0.1:7928")
	require.NoError(t, err)
	assert.ErrorIs(t, m.Rotate(context.Background()), ErrNoGateway)
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
