package engine

import (
	"testing"

	"github.com/metatube-community/metatube-sdk-go/internal/envconfig"
	"github.com/metatube-community/metatube-sdk-go/provider/fanza"
	"github.com/stretchr/testify/require"
)

func TestFANZAURLMatching(t *testing.T) {
	e := New(nil)
	for _, raw := range []string{
		"https://www.dmm.co.jp/mono/dvd/-/detail/=/cid=k9sone740/",
		"https://www.dmm.co.jp/digital/videoa/-/detail/=/cid=midv00047/",
		"https://video.dmm.co.jp/av/content/?id=midv00047",
		"https://VIDEO.DMM.CO.JP/av/content?id=midv00047",
		"https://video.dmm.co.jp/amateur/content/?id=smjx065",
		"https://video.dmm.co.jp/anime/content/?id=196glod00359",
		"https://video.dmm.co.jp/cinema/content/?id=test",
	} {
		t.Run(raw, func(t *testing.T) {
			p, err := e.GetMovieProviderByURL(raw)
			require.NoError(t, err)
			require.Equal(t, fanza.Name, p.Name())
			id, err := p.ParseMovieIDFromURL(raw)
			require.NoError(t, err)
			require.NotEmpty(t, id)
		})
	}
	for _, raw := range []string{
		"https://video.dmm.co.jp.evil.test/av/content/?id=test",
		"https://evilvideo.dmm.co.jp/av/content/?id=test",
		"https://video.dmm.co.jp/not-content/?id=test",
		"ftp://video.dmm.co.jp/av/content/?id=test",
	} {
		_, err := e.GetMovieProviderByURL(raw)
		require.Error(t, err, raw)
	}
	config := envconfig.NewConfig()
	config.Set("priority", "0")
	disabled := New(nil, WithMovieProviderConfig(fanza.Name, config))
	_, err := disabled.GetMovieProviderByURL("https://video.dmm.co.jp/av/content/?id=test")
	require.Error(t, err)
	// Existing providers still use the original host index.
	p, err := e.GetMovieProviderByURL("https://www.javbus.com/ja/SSNI-776")
	require.NoError(t, err)
	require.Equal(t, "JavBus", p.Name())
}
