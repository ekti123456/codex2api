package proxy

import (
	"maps"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type OutboundHeaderDiagnostic struct {
	Headers      map[string]string `json:"headers"`
	TurnMetadata map[string]string `json:"turn_metadata,omitempty"`
}

type outboundBodyDiagnostic struct {
	ClientMetadata map[string]string `json:"client_metadata,omitempty"`
	TurnMetadata   map[string]string `json:"turn_metadata,omitempty"`
	Links          map[string]string `json:"links,omitempty"`
}

type outboundIdentityDiagnostic struct {
	Truncated   bool                      `json:"truncated,omitempty"`
	HTTP        *OutboundHeaderDiagnostic `json:"http,omitempty"`
	WSHandshake *OutboundHeaderDiagnostic `json:"ws_handshake,omitempty"`
	Body        *outboundBodyDiagnostic   `json:"body,omitempty"`
}

func CaptureOutboundIdentityHeaders(headers http.Header) *OutboundHeaderDiagnostic {
	diagnostic := &OutboundHeaderDiagnostic{Headers: make(map[string]string)}
	for _, name := range []string{
		"User-Agent", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features",
		"X-Codex-Installation-Id", "X-Installation-Id", "X-Device-Id", "Oai-Device-Id",
		"Session-Id", "Session_id", "Thread-Id", "Conversation-Id", "Conversation_id",
		"X-Client-Request-Id", "X-Request-Id", "X-Codex-Window-Id", "X-Codex-Parent-Thread-Id",
		"X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent", "Chatgpt-Account-Id",
	} {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		switch name {
		case "User-Agent", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features":
			diagnostic.Headers[name] = diagnosticClientText(values[0])
		case "X-OpenAI-Subagent":
			diagnostic.Headers[name] = diagnosticLabel(values[0])
		default:
			diagnostic.Headers[name] = diagnosticIdentifier(values[0])
		}
		if len(values) > 1 {
			diagnostic.Headers[name+"_multiple"] = "true"
		}
	}
	if raw := headers.Get(codexTurnMetadataHeader); raw != "" {
		if len(raw) > 16384 {
			diagnostic.TurnMetadata = map[string]string{"metadata_status": "too_large"}
		} else {
			diagnostic.TurnMetadata = diagnosticMetadata(gjson.Parse(raw))
		}
	}
	diagnostic.Headers = detachedDiagnosticValues(diagnostic.Headers)
	diagnostic.TurnMetadata = detachedDiagnosticValues(diagnostic.TurnMetadata)
	return diagnostic
}

func detachedDiagnosticValues(values map[string]string) map[string]string {
	for key, value := range values {
		values[key] = strings.Clone(value)
	}
	return values
}

func captureOutboundIdentityBody(body []byte) *outboundBodyDiagnostic {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	diagnostic := &outboundBodyDiagnostic{
		ClientMetadata: detachedDiagnosticValues(diagnosticMetadata(root.Get("client_metadata"))),
		TurnMetadata:   detachedDiagnosticValues(diagnosticMetadata(root.Get("client_metadata.x-codex-turn-metadata"))),
	}
	for _, field := range []string{"prompt_cache_key", "previous_response_id"} {
		if value := root.Get(field); value.Exists() {
			if diagnostic.Links == nil {
				diagnostic.Links = make(map[string]string)
			}
			if value.Type != gjson.String {
				diagnostic.Links[field] = "invalid_type"
			} else if strings.TrimSpace(value.String()) != "" {
				diagnostic.Links[field] = "hash:" + hashRiskIdentity(value.String())
			}
		}
	}
	return diagnostic
}

func (observer *TransportObserver) updateOutboundIdentity(change func(*outboundIdentityDiagnostic)) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		identity := outboundIdentityDiagnostic{}
		if diagnostic.OutboundIdentity != nil {
			identity = *diagnostic.OutboundIdentity
		}
		change(&identity)
		diagnostic.OutboundIdentity = &identity
	})
}

func (observer *TransportObserver) OutboundHTTPIdentity(headers http.Header) {
	if observer == nil {
		return
	}
	diagnostic := CaptureOutboundIdentityHeaders(headers)
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.HTTP = diagnostic
	})
}

func (observer *TransportObserver) OutboundWebsocketHandshake(diagnostic *OutboundHeaderDiagnostic) {
	if observer == nil {
		return
	}
	var copied *OutboundHeaderDiagnostic
	if diagnostic != nil {
		copied = &OutboundHeaderDiagnostic{Headers: maps.Clone(diagnostic.Headers), TurnMetadata: maps.Clone(diagnostic.TurnMetadata)}
	}
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.WSHandshake = copied
	})
}
