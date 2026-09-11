package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestRequestSessionIDPrefixUsesOnlyUnambiguousInboundSession(test *testing.T) {
	const sessionID = "01a09012-b9de-7b40-a04b-612ef4dc3d7d"
	const titleID = "01a0901a-dfa6-7512-80cb-9b7d4b3131ae"
	for _, scenario := range []struct {
		name    string
		headers http.Header
		body    string
		want    string
	}{
		{name: "header", headers: http.Header{"Session-Id": {sessionID}}, want: "01a09012"},
		{name: "legacy header", headers: http.Header{"Session_id": {sessionID}}, want: "01a09012"},
		{name: "uppercase", headers: http.Header{"Session-Id": {strings.ToUpper(sessionID)}}, body: `{"client_metadata":{"session_id":"` + sessionID + `"}}`, want: "01a09012"},
		{name: "body only", body: `{"client_metadata":{"session_id":"` + titleID + `"}}`, want: "01a0901a"},
		{name: "compact", body: `{"client_metadata":{"x-codex-turn-metadata":{"session_id":"` + sessionID + `","request_kind":"compaction"}}}`, want: "01a09012"},
		{name: "encoded metadata", body: `{"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"` + sessionID + `\"}"}}`, want: "01a09012"},
		{name: "WS envelope", body: `{"type":"response.create","response":{"client_metadata":{"session_id":"` + titleID + `"}}}`, want: "01a0901a"},
		{name: "turn header", headers: http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"` + sessionID + `"}`}}, want: "01a09012"},
		{name: "different child thread", body: `{"client_metadata":{"session_id":"` + sessionID + `","thread_id":"` + titleID + `"}}`, want: "01a09012"},
		{name: "missing"},
		{name: "thread is not session", body: `{"client_metadata":{"thread_id":"` + sessionID + `","root_turn_id":"` + sessionID + `"}}`},
		{name: "cache key is not session", body: `{"prompt_cache_key":"` + sessionID + `"}`},
		{name: "hash is not native UUID", headers: http.Header{"Session-Id": {"de1861d585e5991408eb65e3cbf4afec"}}},
		{name: "invalid UUID", headers: http.Header{"Session-Id": {"not-a-session"}}},
		{name: "conflicting carriers", headers: http.Header{"Session-Id": {sessionID}}, body: `{"client_metadata":{"session_id":"` + titleID + `"}}`},
		{name: "conflicting headers", headers: http.Header{"Session-Id": {sessionID, titleID}}},
		{name: "conflicting nested metadata", body: `{"client_metadata":{"session_id":"` + sessionID + `","x-codex-turn-metadata":{"session_id":"` + titleID + `"}}}`},
		{name: "invalid type", body: `{"client_metadata":{"session_id":12345}}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if got := requestSessionIDPrefix(scenario.headers, []byte(scenario.body)); got != scenario.want {
				test.Fatalf("prefix = %q, want %q", got, scenario.want)
			}
		})
	}
}

func TestSessionIDPrefixSurvivesOutboundRewriteAndDiagnosticTruncation(test *testing.T) {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"client_metadata":{"session_id":"01a09012-b9de-7b40-a04b-612ef4dc3d7d"}}`)
	captureUsageRequestIngress(request, body)
	request.Request.Header.Set("Session-Id", "01944eab-8fbf-7d77-a1b4-590ec2955b53")
	state := usageRequestDiagnosticState(request)
	state.Resolved = &usageRequestResolution{RootID: "signed-root-fingerprint", RootState: "resolved", ThreadSource: "thread_title"}
	state.Incoming["large"] = map[string]string{"value": strings.Repeat("x", database.MaxUsageRequestDiagnosticsBytes)}
	input := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(request, input)
	if input.SessionIDPrefix != "01a09012" {
		test.Fatalf("lost original prefix: %q", input.SessionIDPrefix)
	}
	var snapshot usageRequestDiagnostics
	if err := json.Unmarshal([]byte(input.RequestDiagnostics), &snapshot); err != nil {
		test.Fatal(err)
	}
	if !snapshot.Truncated || snapshot.SessionIDPrefix != input.SessionIDPrefix {
		test.Fatalf("truncation lost prefix: %+v", snapshot)
	}
}

func TestPromptSessionWindowDetailCapturesPrefixOnly(test *testing.T) {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	const sessionID = "01a09012-b9de-7b40-a04b-612ef4dc3d7d"
	body := []byte(`{"client_metadata":{"session_id":"` + sessionID + `"}}`)
	detail := promptSessionWindowRequestDetail(request, body, nil, promptfilter.Config{})
	payload, err := json.Marshal(detail)
	if err != nil {
		test.Fatal(err)
	}
	if detail.SessionIDPrefix != "01a09012" || strings.Contains(string(payload), sessionID) {
		test.Fatalf("window should store only prefix: %s", payload)
	}
}
