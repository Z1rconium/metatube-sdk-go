// Package vpngate provides a client for the AimiliVPN (aimili-vpngate)
// gateway management API.
//
// The gateway runs an OpenVPN tunnel towards a VPNGate node and exposes it as
// a local HTTP/HTTPS/SOCKS5 proxy. The management API is used to inspect the
// available nodes and to switch to another one, which is the way to acquire a
// fresh exit IP for region-restricted providers.
package vpngate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCountry is the default ISO country code of the preferred exit IP.
	DefaultCountry = "JP"

	// DefaultTimeout is the default timeout of the management API requests.
	//
	// Note: switching to a new node may take a while, since the gateway has to
	// establish the VPN tunnel and to verify the exit connectivity beforehand.
	DefaultTimeout = 90 * time.Second

	// maxResponseSize is the maximum size of the management API response body.
	maxResponseSize = 16 << 20 // 16MB
)

// ErrUnauthorized is returned when the gateway rejects the credentials.
var ErrUnauthorized = errors.New("vpngate: unauthorized")

// ErrSwitching is returned when the gateway rejects a request because it is
// switching nodes by itself. The request may be retried once it has settled.
var ErrSwitching = errors.New("vpngate: gateway is switching nodes")

// Config is the configuration of the gateway management API client.
type Config struct {
	// URL is the base URL of the gateway management API, which includes the
	// secret path, e.g. http://127.0.0.1:8787/EJsW2EeBo9lY.
	URL string

	// Username is the username of the gateway management backend.
	Username string

	// Password is the password of the gateway management backend.
	Password string

	// Country is the ISO country code of the preferred exit IP.
	Country string

	// Timeout is the timeout of the management API requests.
	Timeout time.Duration
}

// Node represents a VPNGate node managed by the gateway.
type Node struct {
	ID           string `json:"id"`
	IP           string `json:"ip"`
	Country      string `json:"country"`
	CountryShort string `json:"country_short"`
	IPType       string `json:"ip_type"`
	ProbeStatus  string `json:"probe_status"`
	LatencyMS    int    `json:"latency_ms"`
	Active       bool   `json:"active"`
}

// Available reports whether the node has been probed as available.
func (n Node) Available() bool { return n.ProbeStatus == "available" }

// MatchesCountry reports whether the node is located in the given country.
func (n Node) MatchesCountry(country string) bool {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return true
	}
	return strings.EqualFold(n.CountryShort, country)
}

// Status represents the status of the gateway.
type Status struct {
	ActiveNodeID string `json:"active_openvpn_node_id"`
	ProxyOK      bool   `json:"proxy_ok"`
	ProxyIP      string `json:"proxy_ip"`
	IsConnecting bool   `json:"is_connecting"`
	TunnelReady  bool   `json:"tunnel_ready"`
	ProxyReady   bool   `json:"proxy_ready"`
	LocalProxy   string `json:"local_proxy"`
	RoutingMode  string `json:"routing_mode"`
	ForceCountry string `json:"force_country"`
}

// Client is a client of the gateway management API.
type Client struct {
	baseURL  *url.URL
	username string
	password string
	country  string
	client   *http.Client

	mu       sync.Mutex
	loggedIn bool
}

// NewClient returns a management API client of the given configuration.
func NewClient(cfg Config) (*Client, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, errors.New("vpngate: empty url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("vpngate: invalid url %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("vpngate: invalid url %q", rawURL)
	}
	username, password := cfg.Username, cfg.Password
	if u.User != nil {
		if username == "" {
			username = u.User.Username()
		}
		if password == "" {
			if p, ok := u.User.Password(); ok {
				password = p
			}
		}
		u.User = nil // strip the credentials from the URL.
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""

	if username == "" || password == "" {
		return nil, errors.New("vpngate: missing username or password")
	}

	country := strings.ToUpper(strings.TrimSpace(cfg.Country))
	if country == "" {
		country = DefaultCountry
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL:  u,
		username: username,
		password: password,
		country:  country,
		client: &http.Client{
			Timeout: timeout,
			Jar:     jar,
		},
	}, nil
}

// Country returns the preferred country code of the exit IP.
func (c *Client) Country() string { return c.country }

// Nodes returns all the nodes known by the gateway, along with the status of
// the gateway.
func (c *Client) Nodes(ctx context.Context) ([]Node, Status, error) {
	var resp struct {
		Nodes []Node `json:"nodes"`
		State Status `json:"state"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/nodes", nil, &resp); err != nil {
		return nil, Status{}, err
	}
	return resp.Nodes, resp.State, nil
}

// Connect switches the gateway to the given node. It returns after the new
// tunnel has been established and the exit connectivity has been verified.
func (c *Client) Connect(ctx context.Context, nodeID string) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return errors.New("vpngate: empty node id")
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/connect", map[string]string{"id": nodeID}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = resp.Message
		}
		if isSwitchingMessage(msg) {
			return fmt.Errorf("%w: %s", ErrSwitching, msg)
		}
		return fmt.Errorf("vpngate: connect node %s failed: %s", nodeID, msg)
	}
	return nil
}

// do performs a management API request and unmarshals the JSON response into
// out, if out is not nil.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	body, err := c.request(ctx, method, path, in)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
		// The session has expired, re-login and retry once.
		c.invalidateSession()
		if err = c.login(ctx); err != nil {
			return err
		}
		if body, err = c.request(ctx, method, path, in); err != nil {
			return err
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// request performs the request and returns the response body.
func (c *Client) request(ctx context.Context, method, path string, in any) ([]byte, error) {
	var reader io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("%w: %s", ErrSwitching, apiError(body, resp.Status))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vpngate: %s %s: %s", method, path, apiError(body, resp.Status))
	}
	return body, nil
}

// login authenticates with the gateway management backend.
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	if c.username == "" || c.password == "" {
		return errors.New("vpngate: missing username or password")
	}
	buf, err := json.Marshal(map[string]string{
		"username": c.username,
		"password": c.password,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/api/login"), bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	default:
		return fmt.Errorf("vpngate: login: %s", apiError(body, resp.Status))
	}
	c.loggedIn = true
	return nil
}

// invalidateSession drops the current session, so that the next request
// re-authenticates with the gateway.
func (c *Client) invalidateSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedIn = false
}

// endpoint returns the absolute URL of the given API path.
func (c *Client) endpoint(path string) string {
	u := *c.baseURL
	u.Path = c.baseURL.Path + path
	return u.String()
}

// apiError extracts the error message from a management API response body.
func apiError(body []byte, status string) string {
	var resp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Error != "" {
		return resp.Error
	}
	return status
}

// isSwitchingMessage reports whether the given gateway error message indicates
// that the gateway is switching nodes by itself, in which case the request is
// rejected until it has settled.
func isSwitchingMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	for _, s := range []string{
		"switch task is running",
		"switching",
		"reconnecting",
		"正在切换",
		"切换中",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
