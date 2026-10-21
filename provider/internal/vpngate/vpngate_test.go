package vpngate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) *httptest.Server {
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
		if _, err := r.Cookie("session"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
			"nodes": [
				{"id":"JP_1.2.3.4_443_udp","ip":"1.2.3.4","country":"日本","country_short":"JP","probe_status":"available","latency_ms":120,"active":true},
				{"id":"JP_5.6.7.8_443_tcp","ip":"5.6.7.8","country":"日本","country_short":"JP","probe_status":"available","latency_ms":80,"active":false},
				{"id":"US_9.9.9.9_443_udp","ip":"9.9.9.9","country":"美国","country_short":"US","probe_status":"available","latency_ms":10,"active":false},
				{"id":"JP_10.10.10.10_443_udp","ip":"10.10.10.10","country":"日本","country_short":"JP","probe_status":"unavailable","latency_ms":5,"active":false}
			],
			"state": {"proxy_ok": true, "proxy_ip": "1.2.3.4"}
		}`))
	})
	mux.HandleFunc("/secret/api/connect", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var payload map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		switch payload["id"] {
		case "JP_5.6.7.8_443_tcp":
			_, _ = w.Write([]byte(`{"ok":true,"message":"Connected JP_5.6.7.8_443_tcp"}`))
		case "JP_busy_1.1.1.1_443_tcp":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error":"switch task is running"}`))
		case "JP_settling_1.1.1.1_443_tcp":
			_, _ = w.Write([]byte(`{"ok":false,"error":"switch task is running"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"目标节点预检失败"}`))
		}
	})
	return httptest.NewServer(mux)
}

func TestClient_Auth(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Missing credentials.
	_, err := NewClient(Config{URL: srv.URL + "/secret"})
	assert.Error(t, err)

	// Wrong credentials.
	client, err := NewClient(Config{URL: srv.URL + "/secret", Username: "admin", Password: "wrong"})
	require.NoError(t, err)
	_, _, err = client.Nodes(context.Background())
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestClient_Nodes(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Credentials embedded in the URL.
	client, err := NewClient(Config{URL: "http://admin:secret@" + srv.Listener.Addr().String() + "/secret"})
	require.NoError(t, err)
	assert.Equal(t, DefaultCountry, client.Country())

	nodes, status, err := client.Nodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 4)
	assert.True(t, status.ProxyOK)
	assert.Equal(t, "1.2.3.4", status.ProxyIP)

	assert.True(t, nodes[0].Available())
	assert.True(t, nodes[0].Active)
	assert.True(t, nodes[0].MatchesCountry("jp"))
	assert.False(t, nodes[2].MatchesCountry("JP"))
}

func TestClient_Connect(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client, err := NewClient(Config{URL: srv.URL + "/secret", Username: "admin", Password: "secret"})
	require.NoError(t, err)

	require.NoError(t, client.Connect(context.Background(), "JP_5.6.7.8_443_tcp"))
	assert.Error(t, client.Connect(context.Background(), ""))
	assert.Error(t, client.Connect(context.Background(), "JP_unknown"))

	// The gateway is switching nodes by itself, both as a conflict response
	// and as a rejected switch request with a 200 status.
	assert.ErrorIs(t, client.Connect(context.Background(), "JP_busy_1.1.1.1_443_tcp"), ErrSwitching)
	assert.ErrorIs(t, client.Connect(context.Background(), "JP_settling_1.1.1.1_443_tcp"), ErrSwitching)
}
