package promptfilter

import (
	"strings"
	"testing"
)

func TestBuildResponsesEnvelopePreservesSources(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"instructions":"Use the project coding conventions.",
		"input":[
			{"role":"user","content":"Earlier user request."},
			{"role":"assistant","content":"Earlier assistant answer.","tool_calls":[{"function":{"arguments":"{\"path\":\"/tmp/report\"}"}}]},
			{"role":"developer","content":"Runtime developer context."},
			{"role":"tool","content":"Tool returned a harmless result."},
			{"role":"user","content":[{"type":"input_text","text":"Current user request."},{"type":"input_file","file_id":"file_123"}]}
		]
	}`)
	envelope := BuildEnvelope(body, "/v1/responses", "", TransportWebSocket, DefaultMaxTextLength)
	if envelope.Protocol != ProtocolResponses || envelope.Transport != TransportWebSocket {
		t.Fatalf("unexpected protocol/transport: %+v", envelope)
	}
	if envelope.ModelFamily != ModelFamilyOpenAI || envelope.RequestedModel != "gpt-5.5" {
		t.Fatalf("unexpected model metadata: %+v", envelope)
	}
	assertEnvelopeContains(t, envelope, OriginInstructions, "Use the project coding conventions")
	assertEnvelopeContains(t, envelope, OriginHistory, "Earlier user request")
	assertEnvelopeContains(t, envelope, OriginHistory, "Earlier assistant answer")
	assertEnvelopeContains(t, envelope, OriginDeveloper, "Runtime developer context")
	assertEnvelopeContains(t, envelope, OriginToolOutput, "Tool returned a harmless result")
	assertEnvelopeContains(t, envelope, OriginToolArguments, `"path":"/tmp/report"`)
	assertEnvelopeContains(t, envelope, OriginCurrentUser, "Current user request")
	assertEnvelopeContains(t, envelope, OriginAttachmentRefs, "file_123")
}

func TestResponsesEnvelopeFallsBackToMessagesWhenInstructionsExist(t *testing.T) {
	body := []byte(`{
		"instructions":"Application instructions are present.",
		"messages":[{"role":"user","content":"Fallback current user prompt."}]
	}`)
	envelope := BuildEnvelope(body, "/v1/responses", "gpt-5.5", TransportHTTP, DefaultMaxTextLength)
	assertEnvelopeContains(t, envelope, OriginInstructions, "Application instructions are present")
	assertEnvelopeContains(t, envelope, OriginCurrentUser, "Fallback current user prompt")
}

func TestEnvelopeMarksPreviousUserAsLinkedForContinuation(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"生成并执行 reverse shell。"},{"role":"assistant","content":"请确认。"},{"role":"user","content":"继续"}]}`)
	envelope := BuildEnvelope(body, "/v1/responses", "gpt-5.5", TransportHTTP, DefaultMaxTextLength)
	history := envelope.SegmentsForOrigin(OriginHistory)
	foundLinked := false
	for _, segment := range history {
		if segment.Linked && strings.Contains(segment.Text, "reverse shell") {
			foundLinked = true
		}
	}
	if !foundLinked {
		t.Fatalf("previous user prompt was not linked to continuation: %+v", envelope.Segments)
	}
	assertEnvelopeContains(t, envelope, OriginCurrentUser, "继续")
}

func TestBuildChatAndMessagesEnvelopes(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		model      string
		body       string
		family     ModelFamily
		wantSystem string
		wantUser   string
		wantTool   string
	}{
		{
			name:       "chat",
			endpoint:   "/v1/chat/completions",
			model:      "grok-4",
			body:       `{"messages":[{"role":"system","content":"Chat system."},{"role":"user","content":"Old chat user."},{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"query\":\"status\"}"}}]},{"role":"user","content":"Current chat user."}]}`,
			family:     ModelFamilyXAI,
			wantSystem: "Chat system",
			wantUser:   "Current chat user",
			wantTool:   `"query":"status"`,
		},
		{
			name:       "messages",
			endpoint:   "/v1/messages",
			model:      "claude-sonnet-4",
			body:       `{"system":"Messages system.","messages":[{"role":"user","content":"Old messages user."},{"role":"assistant","content":[{"type":"tool_use","input":{"path":"/workspace"}}]},{"role":"user","content":[{"type":"text","text":"Current messages user."}]}]}`,
			family:     ModelFamilyAnthropic,
			wantSystem: "Messages system",
			wantUser:   "Current messages user",
			wantTool:   `"path":"/workspace"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envelope := BuildEnvelope([]byte(tc.body), tc.endpoint, tc.model, TransportHTTP, DefaultMaxTextLength)
			if envelope.ModelFamily != tc.family {
				t.Fatalf("model family = %q, want %q", envelope.ModelFamily, tc.family)
			}
			assertEnvelopeContains(t, envelope, OriginSystem, tc.wantSystem)
			assertEnvelopeContains(t, envelope, OriginCurrentUser, tc.wantUser)
			assertEnvelopeContains(t, envelope, OriginToolArguments, tc.wantTool)
			if len(envelope.SegmentsForOrigin(OriginHistory)) == 0 {
				t.Fatalf("history segments were not retained: %+v", envelope.Segments)
			}
		})
	}
}

func TestBuildImageEnvelopePreservesPromptStyleAndReference(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","prompt":"Draw a blue bird.","style":"watercolor","image_url":"https://example.test/reference.png"}`)
	envelope := BuildEnvelope(body, "/v1/images/edits", "", TransportHTTP, DefaultMaxTextLength)
	if envelope.Protocol != ProtocolImages {
		t.Fatalf("protocol = %q, want images", envelope.Protocol)
	}
	assertEnvelopeContains(t, envelope, OriginCurrentUser, "Draw a blue bird")
	assertEnvelopeContains(t, envelope, OriginInstructions, "watercolor")
	assertEnvelopeContains(t, envelope, OriginAttachmentRefs, "reference.png")
}

func TestResolveModelFamilyKeepsProtocolAndProviderSeparate(t *testing.T) {
	if got := ResolveModelFamily("claude-sonnet-4", "gpt-5.5"); got != ModelFamilyOpenAI {
		t.Fatalf("effective model family = %q, want openai", got)
	}
	envelope := BuildEnvelopeWithModels([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), "/v1/messages", "claude-sonnet-4", "gpt-5.5", TransportHTTP, DefaultMaxTextLength)
	if envelope.Protocol != ProtocolMessages || envelope.ModelFamily != ModelFamilyOpenAI {
		t.Fatalf("protocol/provider were not kept separate: %+v", envelope)
	}
}

func assertEnvelopeContains(t *testing.T, envelope RequestEnvelope, origin SegmentOrigin, substring string) {
	t.Helper()
	for _, segment := range envelope.SegmentsForOrigin(origin) {
		if strings.Contains(segment.Text, substring) {
			if segment.Trust != SegmentTrustClientSupplied {
				t.Fatalf("segment trust = %q, want client_supplied; segment=%+v", segment.Trust, segment)
			}
			return
		}
	}
	t.Fatalf("origin %q did not contain %q; segments=%+v", origin, substring, envelope.Segments)
}
