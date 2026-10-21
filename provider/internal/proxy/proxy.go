// Package proxy provides a proxy manager for region-restricted providers.
//
// The proxy endpoint (e.g. the local listener of an aimili-vpngate gateway)
// stays stable, while the exit IP behind it can be rotated on demand through
// the gateway management API. When the exit IP turns out to be unusable, the
// manager is also able to fall back to the default server egress.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
// again after it has fallen back to the default server egress.
const DefaultFallbackCooldown = time.Minute

// DefaultRecoverTimeout is the maximum duration to wait for the gateway to
// recover by itself, when it rejects a node switch request.
const DefaultRecoverTimeout = 90 * time.Second

// DefaultRecoverGrace is the duration to observe the gateway before switching
// its node on our own, since a gateway with a dead tunnel switches nodes by
// itself and rejects (and penalizes) concurrent switch requests.
const DefaultRecoverGrace = 4 * time.Second

// DefaultRotateTimeout bounds the whole rotation, i.e. the time spent queuing
// behind another rotation, observing the gateway, waiting for it to recover
// and connecting the candidate nodes. Without it, a gateway that accepts the
// connections but never answers could hold the rotation for ten minutes,
// blocking every request of the provider behind it.
const DefaultRotateTimeout = 2 * time.Minute

// recoverPollInterval is the interval of polling the gateway status while
// waiting for it to recover.
const recoverPollInterval = 2 * time.Second

// maxSwitchCandidates is the maximum number of nodes to try when switching.
const maxSwitchCandidates = 3

// routingModeFixedRegion is the gateway routing mode that keeps the exit IP
// in the force country, which is required for a stable region.
const routingModeFixedRegion = "fixed_region"

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

	// fallbackProxy decides the proxy in use while falling back. It defaults
	// to the proxy configured by the environment (e.g. HTTPS_PROXY), i.e. the
	// one that the other providers keep using, or direct egress if none.
	fallbackProxy func(*http.Request) (*url.URL, error)

	mu       sync.Mutex
	gateway  *vpngate.Client
	country  string
	cooldown time.Duration

	recoverTimeout time.Duration
	recoverGrace   time.Duration
	recoverPoll    time.Duration
	rotateTimeout  time.Duration

	// warnOnce reports the gateway configuration of a mismatched country
	// only once, so that it does not flood the log on every rotation.
	warnOnce sync.Once

	// gen counts the completed rotations, so that concurrent callers that
	// have failed with the same exit IP rotate it only once.
	gen       atomic.Uint64
	rotateSem chan struct{}
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
		fallbackProxy:  http.ProxyFromEnvironment,
		cooldown:       DefaultFallbackCooldown,
		recoverTimeout: DefaultRecoverTimeout,
		recoverGrace:   DefaultRecoverGrace,
		recoverPoll:    recoverPollInterval,
		rotateTimeout:  DefaultRotateTimeout,
		rotateSem:      make(chan struct{}, 1),
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
// every HTTP request.
//
// While falling back, it returns the default server egress, which is the
// proxy configured by the environment, if any, or a nil URL for direct egress.
// Once the cooldown has elapsed, the proxy is resumed on the next request. If
// it is still unusable, the failed request falls the manager back again.
func (m *Manager) ProxyFunc(req *http.Request) (*url.URL, error) {
	if m.fallback.Load() {
		if time.Since(m.fellBackTime()) < m.cooldownDuration() {
			if req == nil || req.URL == nil {
				return nil, nil
			}
			return m.fallbackProxy(req) // fall back to the default egress.
		}
		m.Restore() // the cooldown has elapsed, probe the proxy again.
	}
	return m.endpoint.Load(), nil
}

func (m *Manager) cooldownDuration() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldown
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

// HasGateway reports whether a VPN gateway is configured.
func (m *Manager) HasGateway() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gateway != nil
}

// InFallback reports whether the manager is falling back to the default
// server egress.
func (m *Manager) InFallback() bool { return m.fallback.Load() }

// Fallback switches to the default server egress, i.e. the proxy configured by
// the environment, if any, or direct egress.
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
// fallen back to the default server egress and the cooldown has not elapsed yet.
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

// Generation returns the number of the completed rotations. Callers should
// read it before using the exit IP and pass it to RotateFrom, if the exit IP
// turns out to be unusable.
func (m *Manager) Generation() uint64 { return m.gen.Load() }

// Rotate acquires a new exit IP through the VPN gateway, which switches the
// gateway to another node of the preferred country.
func (m *Manager) Rotate(ctx context.Context) error {
	return m.RotateFrom(ctx, m.Generation())
}

// RotateFrom acquires a new exit IP like Rotate, unless the exit IP of the
// given generation has been rotated by another caller in the meantime, in
// which case it returns immediately and the caller should simply retry.
//
// Rotations are serialized, so a burst of failed requests switches the
// gateway only once instead of flooding it with switch requests. The whole
// rotation, including the time spent queuing behind another one, is bounded
// by DefaultRotateTimeout, so that a stalled gateway cannot block the
// provider for minutes.
func (m *Manager) RotateFrom(ctx context.Context, gen uint64) error {
	m.mu.Lock()
	gateway := m.gateway
	country := m.country
	m.mu.Unlock()
	if gateway == nil {
		return ErrNoGateway
	}
	ctx, cancel := context.WithTimeout(ctx, m.rotateTimeoutDuration())
	defer cancel()
	select {
	case m.rotateSem <- struct{}{}:
		defer func() { <-m.rotateSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if m.gen.Load() != gen {
		return nil // another caller has just acquired a new exit IP.
	}
	if err := m.rotate(ctx, gateway, country); err != nil {
		return err
	}
	// The exit IP has been switched, resume the proxy if it was falling back.
	m.gen.Add(1)
	m.Restore()
	return nil
}

func (m *Manager) rotateTimeoutDuration() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rotateTimeout <= 0 {
		return DefaultRotateTimeout
	}
	return m.rotateTimeout
}

func (m *Manager) rotate(ctx context.Context, gateway *vpngate.Client, country string) error {
	// A gateway with a dead tunnel switches the node by itself, and it rejects
	// our switch requests meanwhile, so let it do the job if it is doing so.
	recovering, err := m.observeRecovering(ctx, gateway, country)
	if err != nil {
		return err
	}
	if recovering {
		if m.waitRecovered(ctx, gateway, country) {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
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
	if len(candidates) > maxSwitchCandidates {
		candidates = candidates[:maxSwitchCandidates]
	}
	var lastErr error
	for _, node := range candidates {
		if lastErr = ctx.Err(); lastErr != nil {
			break // the rotation budget has been exhausted.
		}
		if lastErr = gateway.Connect(ctx, node.ID); lastErr == nil {
			return nil
		}
		// The gateway rejects the request if it is busy switching by itself,
		// wait for it to settle instead of trying another node.
		if errors.Is(lastErr, vpngate.ErrSwitching) {
			if m.waitRecovered(ctx, gateway, country) {
				return nil
			}
			return fmt.Errorf("proxy: switch to node %s failed: %w", node.ID, lastErr)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("proxy: no node switch attempted")
	}
	return fmt.Errorf("proxy: switch node failed: %w", lastErr)
}

// observeRecovering observes the gateway for a short grace period, and
// reports whether it is recovering by itself, i.e. it is switching nodes or
// its exit connectivity is down.
func (m *Manager) observeRecovering(ctx context.Context, gateway *vpngate.Client, country string) (bool, error) {
	deadline := time.Now().Add(m.recoverGrace)
	for {
		_, status, err := gateway.Nodes(ctx)
		if err != nil {
			return false, err
		}
		m.warnCountryMismatch(status, country)
		if status.IsConnecting || !status.ProxyOK {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(min(m.recoverPoll, time.Second)):
		}
	}
}

// countryMismatch reports why the gateway status may not provide an exit IP of
// the given country, or an empty string if it looks fine.
func countryMismatch(status vpngate.Status, country string) string {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return ""
	}
	if mode := strings.TrimSpace(status.RoutingMode); mode != "" {
		if !strings.EqualFold(mode, routingModeFixedRegion) {
			return fmt.Sprintf("the routing mode is %q, want %q", mode, routingModeFixedRegion)
		}
	}
	if forced := strings.ToUpper(strings.TrimSpace(status.ForceCountry)); forced != "" && forced != country {
		return fmt.Sprintf("the forced country is %q, want %q", forced, country)
	}
	return ""
}

// warnCountryMismatch warns once about a gateway that may not provide an exit
// IP of the given country, e.g. because it is not in the fixed region mode.
func (m *Manager) warnCountryMismatch(status vpngate.Status, country string) {
	problem := countryMismatch(status, country)
	if problem == "" {
		return
	}
	m.warnOnce.Do(func() {
		log.Printf("[proxy] the VPN gateway may not provide a %s exit IP: %s", country, problem)
	})
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

// fellBackTime returns the time at which the manager fell back to the
// default server egress.
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
