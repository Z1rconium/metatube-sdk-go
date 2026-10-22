package fanza

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metatube-community/metatube-sdk-go/internal/envconfig"
	"github.com/stretchr/testify/require"
)

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureResponse(r *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func maintenanceResponse(r *http.Request) *http.Response {
	resp := fixtureResponse(r, http.StatusFound, "")
	resp.Header.Set("Location", "https://sorry.dmm.co.jp/")
	return resp
}
func TestFANZAMaintenance(t *testing.T) {
	for _, kind := range []string{"detailURL", "reviewURL", "detailID", "reviewID"} {
		t.Run(kind, func(t *testing.T) {
			var gatewayCalls, maintenanceFetches atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gatewayCalls.Add(1)
				http.Error(w, "unexpected gateway call", 500)
			}))
			defer gateway.Close()
			fz := New()
			config := envconfig.NewConfig()
			config.Set("vpngate_url", gateway.URL)
			config.Set("vpngate_username", "test")
			config.Set("vpngate_password", "test")
			require.NoError(t, fz.SetConfig(config))
			rt := maintenanceTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "sorry.dmm.co.jp" {
					maintenanceFetches.Add(1)
					return fixtureResponse(r, 503, "maintenance"), nil
				}
				if r.URL.Hostname() == "api.video.dmm.co.jp" {
					return fixtureResponse(r, 200, `{"data":{}}`), nil
				}
				return maintenanceResponse(r), nil
			})
			fz.Scraper.SetTransport(rt)
			fz.httpClient.Transport = rt
			raw := "https://www.dmm.co.jp/mono/dvd/-/detail/=/cid=k9sone740/"
			var err error
			switch kind {
			case "detailURL":
				_, err = fz.GetMovieInfoByURL(raw)
			case "reviewURL":
				_, err = fz.GetMovieReviewsByURL(raw)
			case "detailID":
				_, err = fz.GetMovieInfoByID("k9sone740")
			case "reviewID":
				_, err = fz.GetMovieReviewsByID("k9sone740")
			}
			require.ErrorIs(t, err, ErrMaintenance)
			require.False(t, fz.isExitUnusable(err))
			require.False(t, fz.proxy.InFallback())
			require.Zero(t, fz.proxy.Generation())
			require.Zero(t, gatewayCalls.Load())
			require.Zero(t, maintenanceFetches.Load())
			require.Contains(t, err.Error(), "FANZA is temporarily under maintenance")
		})
	}
}

func TestFANZAMaintenanceHostIsExact(t *testing.T) {
	fz := New()
	for _, host := range []string{"sorry.dmm.co.jp", "SORRY.DMM.CO.JP", "sorry.dmm.co.jp.evil.test"} {
		u := &url.URL{Scheme: "https", Host: host}
		err := fz.digitalRedirectFunc(&http.Request{URL: u}, nil)
		require.Equal(t, host != "sorry.dmm.co.jp.evil.test", errors.Is(err, ErrMaintenance))
	}
}

func TestFANZANormalDVDParsing(t *testing.T) {
	fz := New()
	fz.Scraper.SetTransport(maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		return fixtureResponse(r, 200, `<html><body><h1 id="title">DVD fixture</h1><img id="package-src-sone740" src="https://images.test/thumb.jpg"><a id="sone740" href="https://images.test/cover.jpg"></a><table><tr><td>品番：</td><td>sone740</td></tr><tr><td>発売日：</td><td>2025/08/01</td></tr><tr><td>メーカー：</td><td>Fixture maker</td></tr></table><span id="performer"><a>Fixture actor</a></span></body></html>`), nil
	}))
	info, err := fz.GetMovieInfoByURL("https://www.dmm.co.jp/mono/dvd/-/detail/=/cid=sone740/")
	require.NoError(t, err)
	require.True(t, info.IsValid())
	require.Equal(t, "sone740", info.ID)
	require.Equal(t, "SONE-740", info.Number)
	require.Equal(t, "DVD fixture", info.Title)
	require.Contains(t, info.Actors, "Fixture actor")
	require.Equal(t, "https://images.test/cover.jpg", info.CoverURL)
	require.Equal(t, "2025-08-01", time.Time(info.ReleaseDate).Format("2006-01-02"))
}

func TestFANZAMaintenanceContinuesSameID(t *testing.T) {
	fz := New()
	var sawMono bool
	rt := maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "api.video.dmm.co.jp" {
			require.True(t, sawMono)
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Contains(t, string(b), "k9sone740")
			return fixtureResponse(r, 200, `{"data":{"ppvContent":{"id":"k9sone740","makerContentId":"SONE-740","floor":"av","title":"Same ID content","packageImage":{"largeUrl":"https://images.test/cover.jpg"}}}}`), nil
		}
		sawMono = true
		return maintenanceResponse(r), nil
	})
	fz.Scraper.SetTransport(rt)
	fz.httpClient.Transport = rt
	info, err := fz.GetMovieInfoByID("k9sone740")
	require.NoError(t, err)
	require.True(t, info.IsValid())
	require.Equal(t, "k9sone740", info.ID)
}

func TestFANZAMaintenanceReviewsContinueSameID(t *testing.T) {
	fz := New()
	var sawMono bool
	rt := maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "api.video.dmm.co.jp" {
			require.True(t, sawMono)
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Contains(t, string(b), "k9sone740")
			return fixtureResponse(r, 200, `{"data":{"reviews":{"items":[{"nickname":"Fixture reviewer","comment":"Fixture comment","rating":5}]}}}`), nil
		}
		sawMono = true
		return maintenanceResponse(r), nil
	})
	fz.Scraper.SetTransport(rt)
	fz.httpClient.Transport = rt
	reviews, err := fz.GetMovieReviewsByID("k9sone740")
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	require.True(t, reviews[0].IsValid())
}
