package admin

import (
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionAutoLockAdminSettings(t *testing.T) {
	h := &Handler{db: newTestAdminDB(t)}
	router := gin.New()
	h.RegisterRoutes(router)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, "/api/admin/session-errors/auto-lock", nil))
		require.Contains(t, []int{401, 503}, w.Code)
	}
	for _, item := range []struct {
		body string
		code int
	}{
		{`{}`, 400}, {`{"enabled":true,"threshold":0}`, 400}, {`{"enabled":true,"threshold":10001}`, 400}, {`{"enabled":true,"threshold":1.5}`, 400}, {`{"enabled":true,"threshold":500}`, 200}, {`{"enabled":false,"threshold":3}`, 200},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(item.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.SetSessionAutoLockSettings(c)
		require.Equal(t, item.code, w.Code, w.Body.String())
	}
	require.False(t, h.db.GetSessionAutoLockSettings().Enabled)
}
