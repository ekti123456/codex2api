package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
)

func configureCodexAuxiliaryIdentity(test *testing.T, clientName, mode string) {
	test.Helper()
	previous := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(fmt.Sprintf(`{"client_name":%q,"client_version":"0.153.4","os_name":"Windows","os_version":"10.0.19045","arch":"x86_64","terminal":"unknown"}`, clientName))
	if err != nil {
		test.Fatal(err)
	}
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: mode, CodexUserAgentConfig: normalized})
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestCodexAuxiliaryEndpointsUseConfiguredIdentity(test *testing.T) {
	test.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, clientName := range []string{"codex-tui", "Codex Desktop"} {
		test.Run(clientName, func(test *testing.T) {
			configureCodexAuxiliaryIdentity(test, clientName, ClientCompatModeForce)
			account := &auth.Account{DBID: 9811, AccessToken: "test-access-token", AccountID: "test-account"}
			wantUA, wantVersion, wantOriginator := ResolveCodexOutboundClientIdentity(account, "", nil, nil)
			if !strings.Contains(wantUA, "(Windows 10.0.19045; x86_64)") || wantOriginator != clientName {
				test.Fatalf("unexpected configured identity: %q / %q", wantUA, wantOriginator)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				for name, expected := range map[string]string{
					"User-Agent": wantUA, "Version": wantVersion, "Originator": wantOriginator,
					"Authorization": "Bearer test-access-token", "Chatgpt-Account-Id": "test-account",
				} {
					if actual := request.Header.Get(name); actual != expected {
						test.Errorf("%s: %s = %q, want %q", request.URL.Path, name, actual, expected)
					}
				}
				if request.URL.Path == "/models" && request.URL.Query().Get("client_version") != wantVersion {
					test.Errorf("models version query = %q, want %q", request.URL.RawQuery, wantVersion)
				}
				for _, name := range []string{"Session-Id", "Thread-Id", "X-Codex-Turn-Metadata", "Cookie"} {
					if request.Header.Get(name) != "" {
						test.Errorf("%s: unexpected header %s", request.URL.Path, name)
					}
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{}`))
			}))
			defer server.Close()
			tests := []struct {
				name string
				run  func() error
			}{
				{"models", func() error {
					_, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL+"/models", "", "")
					return err
				}},
				{"usage", func() error {
					_, _, err := queryWhamUsageWithURL(context.Background(), account, "", server.URL+"/usage")
					return err
				}},
				{"credits", func() error {
					_, _, err := queryWhamResetCreditsWithURL(context.Background(), account, "", server.URL+"/credits")
					return err
				}},
				{"consume", func() error {
					_, _, err := consumeResetCreditWithURL(context.Background(), account, "", server.URL+"/consume", "test-redeem-id")
					return err
				}},
				{"daily", func() error {
					_, _, err := queryWhamDailyUsageWithURL(context.Background(), account, "", server.URL+"/daily", "2026-09-09", "2026-09-10")
					return err
				}},
				{"breakdown", func() error {
					_, _, err := queryWhamDailyTokenBreakdownWithURL(context.Background(), account, "", server.URL+"/breakdown", "2026-09-09", "2026-09-10")
					return err
				}},
			}
			for _, scenario := range tests {
				test.Run(scenario.name, func(test *testing.T) {
					if err := scenario.run(); err != nil {
						test.Fatal(err)
					}
				})
			}
			if actual := requests.Load(); actual != int32(len(tests)) {
				test.Fatalf("requests = %d, want %d", actual, len(tests))
			}
		})
	}
}

func TestCodexAuxiliaryIdentityOverrideDoesNotForwardUnrelatedHeaders(test *testing.T) {
	configureCodexAuxiliaryIdentity(test, "codex-tui", ClientCompatModeForce)
	account := &auth.Account{DBID: 9812, CustomHeaders: map[string]string{
		"user-agent": "custom-client/7.0", "version": "7.0", "originator": "custom-client",
		"Authorization": "Bearer unrelated-secret", "Cookie": "private-cookie", "X-Codex-Window-Id": "private-window",
	}}
	request := httptest.NewRequest(http.MethodGet, "/models", nil)
	request.Header.Set("Authorization", "Bearer selected-account")
	applyCodexAuxiliaryClientHeaders(request, account, "", nil, nil, "0.140.0")
	for name, expected := range map[string]string{
		"User-Agent": "custom-client/7.0", "Version": "7.0", "Originator": "custom-client",
		"Authorization": "Bearer selected-account", "Cookie": "", "X-Codex-Window-Id": "",
	} {
		if actual := request.Header.Get(name); actual != expected {
			test.Errorf("%s = %q, want %q", name, actual, expected)
		}
	}
}

func TestCodexAuxiliaryModelsKeepRequestedVersionAndConfiguredPlatform(test *testing.T) {
	test.Setenv("CODEX_TRANSPORT_MODE", "standard")
	configureCodexAuxiliaryIdentity(test, "codex-tui", ClientCompatModeForce)
	account := &auth.Account{DBID: 9813, AccessToken: "test-token"}
	type capturedModelRequest struct {
		headers http.Header
		version string
	}
	captured := make(chan capturedModelRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured <- capturedModelRequest{headers: request.Header.Clone(), version: request.URL.Query().Get("client_version")}
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	for _, customVersion := range []string{"", "0.153.4"} {
		account.CustomHeaders = map[string]string{}
		wantVersion := "0.140.0"
		if customVersion != "" {
			account.CustomHeaders["Version"] = customVersion
			account.CustomHeaders["User-Agent"] = "codex-tui/0.153.4 (Windows 10.0.19045; x86_64) unknown (codex-tui; 0.153.4)"
			wantVersion = customVersion
		}
		if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", ""); err != nil {
			test.Fatal(err)
		}
		observed := <-captured
		wantUA := fmt.Sprintf("codex-tui/%s (Windows 10.0.19045; x86_64) unknown (codex-tui; %s)", wantVersion, wantVersion)
		if observed.headers.Get("User-Agent") != wantUA || observed.headers.Get("Version") != wantVersion || observed.version != wantVersion {
			test.Fatalf("model identity = %q / %q / %q, want %q / %q", observed.headers.Get("User-Agent"), observed.headers.Get("Version"), observed.version, wantUA, wantVersion)
		}
	}
}

func TestCodexAlphaSearchAndLiveUseConfiguredAndIncomingOriginator(test *testing.T) {
	test.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, mode := range []string{ClientCompatModeForce, ClientCompatModePreserve} {
		test.Run(mode, func(test *testing.T) {
			configureCodexAuxiliaryIdentity(test, "Codex Desktop", mode)
			account := &auth.Account{DBID: 9814, AccessToken: "test-token", AccountID: "test-account"}
			downstream := http.Header{}
			downstream.Set("User-Agent", "Codex Desktop/0.153.4 (Windows 10.0.19045; x86_64) unknown (Codex Desktop; 26.901.41123)")
			downstream.Set("Originator", "Codex Desktop")
			wantUA, wantVersion, wantOriginator := ResolveCodexOutboundClientIdentity(account, "", nil, downstream)
			if wantOriginator != "Codex Desktop" {
				test.Fatalf("originator = %q, want Codex Desktop", wantOriginator)
			}
			captured := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				captured <- request.Header.Clone()
				_, _ = writer.Write([]byte(`{"output":[]}`))
			}))
			defer server.Close()
			previousURL := codexAlphaSearchURLForTest
			codexAlphaSearchURLForTest = server.URL
			test.Cleanup(func() { codexAlphaSearchURLForTest = previousURL })
			if _, err := ForwardCodexAlphaSearch(context.Background(), account, "", []byte(`{"model":"gpt-5.6-sol"}`), downstream, nil, ""); err != nil {
				test.Fatal(err)
			}
			observed := <-captured
			live := httptest.NewRequest(http.MethodPost, "/live", nil)
			handler := &Handler{}
			handler.applyLiveUpstreamHeaders(live, account, "test-attestation", downstream, "")
			for name, headers := range map[string]http.Header{"search": observed, "live": live.Header} {
				if headers.Get("User-Agent") != wantUA || headers.Get("Version") != wantVersion || headers.Get("Originator") != wantOriginator {
					test.Errorf("%s identity = %q / %q / %q", name, headers.Get("User-Agent"), headers.Get("Version"), headers.Get("Originator"))
				}
			}
			if live.Header.Get("OpenAI-Alpha") != "quicksilver=v2" || live.Header.Get("Accept") != "application/sdp" {
				test.Fatal("Live-specific headers were changed")
			}
		})
	}
}
