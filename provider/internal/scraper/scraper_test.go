package scraper

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"
)

func TestSetProxyPreservesTransport(t *testing.T) {
	original := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxConnsPerHost:     2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     5 * time.Minute,
	}
	s := NewDefaultScraper("test", "https://example.com", 1, language.Japanese, WithTransport(original))
	require.Same(t, original, s.Transport())
	t.Cleanup(s.closeIdleConnections)

	require.NoError(t, s.SetProxy("http://127.0.0.1:7928"))
	transport := s.Transport().(*http.Transport)
	assert.NotSame(t, original, transport)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
	assert.Equal(t, 2, transport.MaxConnsPerHost)
	assert.Equal(t, 2, transport.MaxIdleConnsPerHost)
	assert.Equal(t, 5*time.Minute, transport.IdleConnTimeout)
	assert.True(t, transport.DisableKeepAlives)
	u, err := transport.Proxy(httptest.NewRequest(http.MethodGet, "https://example.com", nil))
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7928", u.String())
	assert.Nil(t, original.Proxy)
	assert.False(t, original.DisableKeepAlives)

	// Switching again retains the custom TLS settings in real requests.
	s.SetProxyFunc(func(*http.Request) (*url.URL, error) { return nil, nil })
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := s.ClonedCollector()
	var body string
	c.OnResponse(func(r *colly.Response) { body = string(r.Body) })
	require.NoError(t, c.Visit(srv.URL))
	assert.Equal(t, "ok", body)
}
