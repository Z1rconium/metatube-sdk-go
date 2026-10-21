// Package proxy provides a proxy manager for region-restricted providers.
//
// The proxy endpoint (e.g. the local listener of an aimili-vpngate gateway)
// stays stable, while the exit IP behind it can be rotated on demand through
// the gateway management API. When the exit IP turns out to be unusable, the
// manager is also able to fall back to direct server egress.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/metatube-community/metatube-sdk-go/provider/internal/vpngate"
)

// DefaultFallbackCooldown is the minimum duration before the proxy is probed
// again after it has fallen back to direct server egress.
const DefaultFallbackCooldown = 5 * time.Minute

// DefaultRecoverTimeout is the maximum duration to wait for the gateway to
// recover by itself, when it rejects a node switch request.
const DefaultRecoverTimeout = 90 * time.Second

// recoverPollInterval is the interval of polling the gateway status while
// waiting for it to recover.
const recoverPollInterval = 2 * time.Second

// ErrNoGateway is returned when no VPN gateway is configured.
var ErrNoGateway = errors.New("proxy: no vpn gateway configured")

// StatusError is an error for unexpected HTTP response statuses.
type StatusError struct {
	Code   int
	Status string
}

// Error implements the error interface.
func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected response status: %s", e.Status)
}

// CheckStatus returns a round tripper that turns unexpected HTTP response
// statuses into errors.
//
// It is useful for API clients that silently ignore the response status,
// which would otherwise hide a dead VPN tunnel behind a decoding error.
func CheckStatus(rt http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
			resp.Body.Close()
			return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status}
		}
		return resp, nil
	})
}

// roundTripperFunc is an adapter to allow the use of ordinary functions as
// HTTP round trippers.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements the http.RoundTripper interface.
func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Manager manages the outbound proxy of a region-restricted provider.
type Manager struct {
	endpoint  atomic.Pointer[url.URL]
	fallback  atomic.Bool
	fellBack  atomic.Int64 // unix nanoseconds
	transport *http.Transport

	mu       sync.Mutex
	gateway  *vpngate.Client
	country  string
	cooldown time.Duration

	recoverTimeout time.Duration
	recoverPoll    time.Duration
}

// New returns a proxy manager for the given proxy endpoint, which may be an
// HTTP, HTTPS, SOCKS5 or SOCKS5H proxy URL.
func New(rawURL string) (*Manager, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("proxy: empty url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid url %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy: invalid url %q", rawURL)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy: unsupported proxy scheme %q", u.Scheme)
	}
	m := &Manager{
		cooldown:       DefaultFallbackCooldown,
		recoverTimeout: DefaultRecoverTimeout,
		recoverPoll:    recoverPollInterval,
	}
	m.endpoint.Store(u)
	transport := &http.Transport{Proxy: m.ProxyFunc, DisableKeepAlives: true}
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = dt.Clone()
		transport.Proxy = m.ProxyFunc
		transport.DisableKeepAlives = true
	}
	m.transport = transport
	return m, nil
}

// SetGateway sets the VPN gateway that is used to rotate the exit IP.
func (m *Manager) SetGateway(gateway *vpngate.Client, country string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateway = gateway
	if country = strings.TrimSpace(country); country != "" {
		m.country = strings.ToUpper(country)
	}
}

// Country returns the preferred country code of the exit IP.
func (m *Manager) Country() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.country
}

// ProxyFunc returns the proxy function of the manager, which is invoked for
// every HTTP request. A nil URL indicates direct server egress.
func (m *Manager) ProxyFunc(*http.Request) (*url.URL, error) {
	if m.fallback.Load() {
		return nil, nil // fall back to direct server egress.
	}
	return m.endpoint.Load(), nil
}

// Transport returns the HTTP transport bound to the manager.
func (m *Manager) Transport() *http.Transport { return m.transport }

// Endpoint returns the proxy endpoint in use.
func (m *Manager) Endpoint() string {
	if u := m.endpoint.Load(); u != nil {
		return u.String()
	}
	return ""
}

// InFallback reports whether the manager is falling back to direct egress.
func (m *Manager) InFallback() bool { return m.fallback.Load() }

// Fallback switches to direct server egress.
func (m *Manager) Fallback() {
	if m.fallback.Swap(true) {
		return
	}
	m.fellBack.Store(time.Now().UnixNano())
	m.transport.CloseIdleConnections()
}

// Restore switches back to the proxy endpoint.
func (m *Manager) Restore() {
	if !m.fallback.Swap(false) {
		return
	}
	m.transport.CloseIdleConnections()
}

// CanRotate reports whether a new exit IP can be acquired on demand.
//
// It returns false if no VPN gateway is configured, or if the proxy has just
// fallen back to direct server egress and the cooldown has not elapsed yet.
func (m *Manager) CanRotate() bool {
	m.mu.Lock()
	gateway := m.gateway
	cooldown := m.cooldown
	m.mu.Unlock()
	if gateway == nil {
		return false
	}
	if m.fallback.Load() {
		// Only probe the proxy again after the cooldown.
		return time.Since(m.fellBackTime()) >= cooldown
	}
	return true
}

// Rotate acquires a new exit IP through the VPN gateway, which switches the
// gateway to another node of the preferred country.
func (m *Manager) Rotate(ctx context.Context) error {
	m.mu.Lock()
	gateway := m.gateway
	country := m.country
	m.mu.Unlock()
	if gateway == nil {
		return ErrNoGateway
	}
	nodes, _, err := gateway.Nodes(ctx)
	if err != nil {
		return err
	}
	candidates := make([]vpngate.Node, 0, len(nodes))
	for _, node := range nodes {
		if !node.Available() || node.Active {
			continue
		}
		if !node.MatchesCountry(country) {
			continue
		}
		candidates = append(candidates, node)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("proxy: no available %s node found", country)
	}
	// Prefer the node with the lowest latency.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].LatencyMS < candidates[j].LatencyMS
	})
	node := candidates[0]
	if err = gateway.Connect(ctx, node.ID); err != nil {
		// The gateway rejects the request if it is busy reconnecting by
		// itself, e.g. after the tunnel has just died. Wait for it to settle
		// on a node of the preferred country instead of giving up.
		if !m.waitRecovered(ctx, gateway, country) {
			return fmt.Errorf("proxy: switch to node %s failed: %w", node.ID, err)
		}
	}
	// The exit IP has been switched, resume the proxy if it was falling back.
	m.Restore()
	return nil
}

// waitRecovered waits until the gateway is connected to a usable node of the
// given country, and reports whether it recovered in time.
func (m *Manager) waitRecovered(ctx context.Context, gateway *vpngate.Client, country string) bool {
	ctx, cancel := context.WithTimeout(ctx, m.recoverTimeout)
	defer cancel()
	ticker := time.NewTicker(m.recoverPoll)
	defer ticker.Stop()
	for {
		if nodes, status, err := gateway.Nodes(ctx); err == nil && !status.IsConnecting && status.ProxyOK {
			for _, node := range nodes {
				if node.Active && node.MatchesCountry(country) {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// fellBackTime returns the time at which the manager fell back to direct
// server egress.
func (m *Manager) fellBackTime() time.Time {
	nsec := m.fellBack.Load()
	if nsec == 0 {
		return time.Time{}
	}
	return time.Unix(0, nsec)
}

// IsTransportError reports whether the given error is caused by an unusable
// network path, e.g. a dead VPN tunnel or an unreachable proxy.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	// A gateway error response of the VPN gateway proxy, which indicates that
	// the underlying VPN tunnel is down.
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.Code {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	// The VPN gateway responds with a gateway error when the underlying VPN
	// tunnel is down, e.g. "Bad Gateway".
	msg := err.Error()
	for _, s := range []string{
		"Bad Gateway",
		"Service Unavailable",
		"Gateway Timeout",
		"connection refused",
		"connection reset",
		"no such host",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
