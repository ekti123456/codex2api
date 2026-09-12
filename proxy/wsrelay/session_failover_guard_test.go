package wsrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestWebsocketSessionFailoverRejectsGenerationZeroAfterHandshakeWait(test *testing.T) {
	previousSettings, previousResin, previousExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	test.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previousSettings)
		proxy.SetResinConfig(previousResin)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexSessionFailoverEnabled, settings.CodexForceWebsocket = true, true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "guard.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	memory := cache.NewMemory(100)
	test.Cleanup(func() { require.NoError(test, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	test.Cleanup(store.Stop)
	owner := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "owner", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	target := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "target", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, Disabled: 1}
	store.AddAccount(owner)
	store.AddAccount(target)
	handler := proxy.NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(memory)
	const root = "01a09351-7b81-7ae0-afd0-225e178ea131"
	digest := sha256.Sum256([]byte(root))
	rootKey := hex.EncodeToString(digest[:12])
	var frames atomic.Int32
	migrated := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := owner
		for index, selected := range []*auth.Account{target, owner} {
			_, _, err := db.SwitchSessionContinuityAccount(request.Context(), database.SessionAccountFailover{RootKey: rootKey, ExpectedAccountID: current.ID(), AccountID: selected.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: root})
			if err != nil {
				migrated <- err
				writer.WriteHeader(500)
				return
			}
			current = selected
		}
		migrated <- nil
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_, _, err = connection.ReadMessage()
		if err == nil {
			frames.Add(1)
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "guard-test"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":"plaintext","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:0","window_number":0}}}`, root, root, root, root, root))
	recorder := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(recorder)
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Request.Header.Set("Authorization", "Bearer test-user-key")
	ctx, cancel := context.WithTimeout(request.Request.Context(), 5*time.Second)
	defer cancel()
	request.Request = request.Request.WithContext(ctx)
	handler.Responses(request)
	select {
	case err := <-migrated:
		require.NoError(test, err)
	default:
		test.Fatal("handshake did not reach migration hook")
	}
	require.Contains(test, recorder.Body.String(), "codex_session_identity_unavailable")
	require.Zero(test, frames.Load())
}
