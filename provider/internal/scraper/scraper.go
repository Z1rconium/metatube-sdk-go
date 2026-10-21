package scraper

import (
	"net/http"
	"net/url"
	"time"

	"github.com/gocolly/colly/v2"
	"go.uber.org/atomic"
	"golang.org/x/text/language"

	"github.com/metatube-community/metatube-sdk-go/provider"
)

var (
	_ provider.Provider             = (*Scraper)(nil)
	_ provider.ProxySetter          = (*Scraper)(nil)
	_ provider.RequestTimeoutSetter = (*Scraper)(nil)
)

// Scraper implements the basic Provider interface.
type Scraper struct {
	name      string
	baseURL   *url.URL
	priority  *atomic.Float64
	language  language.Tag
	c         *colly.Collector
	transport http.RoundTripper
}

// NewScraper returns a *Scraper that implements provider.Provider.
func NewScraper(name, base string, priority float64, lang language.Tag, opts ...Option) *Scraper {
	baseURL, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	s := &Scraper{
		name:     name,
		baseURL:  baseURL,
		priority: atomic.NewFloat64(priority),
		language: lang,
		c:        colly.NewCollector(),
	}
	for _, opt := range opts {
		// Apply options.
		if err := opt(s); err != nil {
			panic(err)
		}
	}
	return s
}

// NewDefaultScraper returns a *Scraper with default options enabled.
func NewDefaultScraper(name, baseURL string, priority float64, lang language.Tag, opts ...Option) *Scraper {
	return NewScraper(name, baseURL, priority, lang, append([]Option{
		WithAllowURLRevisit(),
		WithIgnoreRobotsTxt(),
		WithRandomUserAgent(),
	}, opts...)...)
}

func (s *Scraper) Name() string { return s.name }

func (s *Scraper) URL() *url.URL { return s.baseURL }

func (s *Scraper) Priority() float64 { return s.priority.Load() }

func (s *Scraper) SetPriority(v float64) { s.priority.Store(v) }

func (s *Scraper) Language() language.Tag { return s.language }

func (s *Scraper) NormalizeMovieID(id string) string { return id /* AS IS */ }

func (s *Scraper) ParseMovieIDFromURL(string) (string, error) { panic("unimplemented") }

func (s *Scraper) NormalizeActorID(id string) string { return id /* AS IS */ }

func (s *Scraper) ParseActorIDFromURL(string) (string, error) { panic("unimplemented") }

// ClonedCollector returns cloned internal collector.
func (s *Scraper) ClonedCollector() *colly.Collector { return s.c.Clone() }

// SetProxy sets http or socks5 proxy for HTTP requests.
func (s *Scraper) SetProxy(proxyURL string) error {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return err
	}
	s.SetProxyFunc(http.ProxyURL(u))
	return nil
}

// SetProxyFunc sets a proxy function for HTTP requests.
//
// Unlike SetProxy, the given function is invoked for every request, which
// makes it possible to switch the proxy at runtime, e.g. to rotate the exit
// IP of a VPN gateway or to fall back to direct server egress.
//
// Note that the transport resolves the proxy per connection, so the keep
// alive connections are disabled to make a switch take effect immediately.
// Prefer SetTransport if the transport itself manages the switching.
func (s *Scraper) SetProxyFunc(f func(*http.Request) (*url.URL, error)) {
	transport := &http.Transport{Proxy: f, DisableKeepAlives: true}
	base := s.transport
	if base == nil {
		base = http.DefaultTransport
	}
	if dt, ok := base.(*http.Transport); ok {
		transport = dt.Clone()
		transport.Proxy = f
		transport.DisableKeepAlives = true
	}
	s.SetTransport(transport)
}

// SetTransport sets the transport for HTTP requests, e.g. one that is shared
// with a runtime-switchable proxy manager, so that switching the proxy closes
// the idle connections of the scraper as well.
//
// The idle connections of the previous transport, if it has any, are closed.
func (s *Scraper) SetTransport(transport http.RoundTripper) {
	s.closeIdleConnections()
	s.transport = transport
	s.c.WithTransport(transport)
}

// Transport returns the transport in use, or nil if the default one is used.
func (s *Scraper) Transport() http.RoundTripper { return s.transport }

// closeIdleConnections closes the idle connections of the current transport.
func (s *Scraper) closeIdleConnections() {
	if t, ok := s.transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

// SetRequestTimeout sets timeout for HTTP requests.
func (s *Scraper) SetRequestTimeout(timeout time.Duration) { s.c.SetRequestTimeout(timeout) }
