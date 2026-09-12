package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func useCodexHTTPTestAccounts(test *testing.T, accounts ...*auth.Account) {
	test.Helper()
	previousResin := GetResinConfig()
	previousSettings := CurrentRuntimeSettings()
	test.Cleanup(func() { SetResinConfig(previousResin); ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	routes := make(map[string]*httputil.ReverseProxy)
	for _, account := range accounts {
		target, err := url.Parse(account.BaseURL)
		require.NoError(test, err)
		proxy := httputil.NewSingleHostReverseProxy(target)
		director := proxy.Director
		proxy.Director = func(request *http.Request) {
			director(request)
			path := "/v1/responses"
			if strings.HasSuffix(request.URL.Path, "/compact") {
				path += "/compact"
			}
			request.URL.Path = path
		}
		proxy.ModifyResponse = func(response *http.Response) error {
			if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "application/json") || strings.HasSuffix(response.Request.URL.Path, "/compact") {
				return nil
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				return err
			}
			response.Body = io.NopCloser(strings.NewReader(fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":%s}\n\n", body)))
			response.ContentLength = -1
			response.Header.Del("Content-Length")
			response.Header.Set("Content-Type", "text/event-stream")
			return nil
		}
		account.AccessToken = account.APIKey
		routes["Bearer "+account.AccessToken] = proxy
		account.UpstreamType, account.APIKey, account.BaseURL = "", "", ""
		account.Status = auth.StatusReady
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if proxy := routes[request.Header.Get("Authorization")]; proxy != nil {
			proxy.ServeHTTP(writer, request)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	test.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "codex-test"})
}
