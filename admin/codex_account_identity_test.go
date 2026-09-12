package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexAccountIdentitySingleAndBatchTests(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	previousResin, previousSettings := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
	test.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousSettings)
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(request.Body)
		received <- capture{request.Header.Clone(), body}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"test-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "account-test"})
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "tests.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.5"})
	test.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "test-token", AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", Status: auth.StatusReady}
	store.AddAccount(account)
	handler := &Handler{store: store, db: db}
	response := serveCodexDiagnosticsTest(handler)
	require.Equal(test, http.StatusOK, response.Code)
	require.Contains(test, response.Body.String(), `"success":true`)
	status, message := handler.runSingleBatchTest(context.Background(), account)
	require.Equal(test, "success", status, message)
	require.Len(test, received, 2)
	var previousSession string
	for range 2 {
		sent := <-received
		session := sent.headers.Get("Session-Id")
		require.NotEmpty(test, session)
		require.NotEqual(test, previousSession, session)
		previousSession = session
		require.Equal(test, session, sent.headers.Get("Thread-Id"))
		require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
		require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.thread_id").String())
		require.NotEqual(test, session, gjson.GetBytes(sent.body, "prompt_cache_key").String())
		require.Equal(test, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
	}
}
