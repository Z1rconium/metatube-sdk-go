package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/metatube-community/metatube-sdk-go/provider/fanza"
	"github.com/stretchr/testify/require"
)

func TestFANZAMaintenanceAPIError(t *testing.T) {
	for _, path := range []string{"/v1/movies/FANZA/test", "/v1/reviews/FANZA/test"} {
		r := gin.New()
		r.GET(path, func(c *gin.Context) { abortWithError(c, fmt.Errorf("redirect: %w", fanza.ErrMaintenance)) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.JSONEq(t, `{"error":{"code":503,"message":"FANZA is temporarily under maintenance"}}`, w.Body.String())
	}
}
