package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionErrorRoutesRequireAdminAuthentication(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	router := gin.New()
	handler.RegisterRoutes(router)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/admin/session-errors"
		if method == http.MethodPost {
			path += "/blacklist"
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(`{"keys":[],"locked":true}`)))
		require.Contains(test, []int{http.StatusUnauthorized, http.StatusServiceUnavailable}, response.Code)
	}
}

func TestSessionErrorAdminBatchValidationAndAtomicFailure(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	key := strings.Repeat("a", 64)
	require.True(test, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: database.SessionErrorIdentity{Key: key, Kind: "api_key", Platform: "codex-local", UserID: "17", SessionID: "root"}, CreatedAt: time.Now()}))
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	for _, item := range []struct {
		body   string
		status int
	}{
		{`{"keys":["` + key + `"]}`, 400},
		{`{"keys":["` + key + `","` + key + `"],"locked":true}`, 400},
		{`{"keys":["` + key + `","` + strings.Repeat("b", 64) + `"],"locked":true}`, 404},
	} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/session-errors/blacklist", strings.NewReader(item.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.SetSessionBlacklist(ctx)
		require.Equal(test, item.status, response.Code, response.Body.String())
		lockedBy, err := handler.db.SessionBlacklistStatus(context.Background(), key, "")
		require.NoError(test, err)
		require.Empty(test, lockedBy)
	}
	for _, query := range []string{"limit=101", "locked=invalid", "lock_state=invalid", "cursor=bad", "user_id=" + strings.Repeat("x", 256)} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/session-errors?"+query, nil)
		handler.GetSessionErrors(ctx)
		require.Equal(test, http.StatusBadRequest, response.Code)
	}
}

func TestSessionErrorAdminDefaultsToUnlockedAndSupportsLockFilter(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	lockedKey, openKey := strings.Repeat("c", 64), strings.Repeat("d", 64)
	for _, key := range []string{lockedKey, openKey} {
		require.True(test, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: database.SessionErrorIdentity{Key: key, UserID: "17", SessionID: key}, CreatedAt: time.Now()}))
	}
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(test, handler.db.SetSessionBlacklist(context.Background(), []string{lockedKey}, true))
	for _, sample := range []struct{ query, expected string }{{"", openKey}, {"lock_state=unlocked", openKey}, {"lock_state=locked", lockedKey}, {"locked=true", lockedKey}} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/session-errors?"+sample.query, nil)
		handler.GetSessionErrors(ctx)
		require.Equal(test, http.StatusOK, response.Code, response.Body.String())
		var page database.SessionErrorPage
		require.NoError(test, json.Unmarshal(response.Body.Bytes(), &page))
		require.Equal(test, int64(1), page.Groups)
		require.Len(test, page.Items, 1)
		require.Equal(test, sample.expected, page.Items[0].Identity.Key)
	}
}
