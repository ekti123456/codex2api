package proxy

import (
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// Observe only declarations in the upstream envelope, never text, tool output,
// or a model synthesized by a protocol translator. This has no billing effect.
func observeUpstreamResponseModel(attempt *upstreamTraceAttempt, payload []byte, event string) {
	model := ""
	for _, path := range []string{"response.model", "model"} {
		value := gjson.GetBytes(payload, path)
		if value.Type == gjson.String {
			model = strings.TrimSpace(value.String())
			if model != "" {
				break
			}
		}
	}
	if model == "" || len(model) > 200 || !gjson.ValidBytes(payload) {
		return
	}
	// Model identifiers only; do not let an arbitrary upstream value inject
	// control characters, credentials, or response prose into diagnostic logs.
	if strings.HasPrefix(model, "sk-") || strings.HasPrefix(model, "eyJ") {
		return
	}
	for _, char := range model {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-_.:/", char)) {
			return
		}
	}
	diagnostic := &attempt.transport
	if previous := diagnostic.ResponseModel; previous != "" && !strings.EqualFold(previous, model) {
		diagnostic.ResponseModelConflict = true
	}
	terminal := false
	switch event {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		terminal = true
	}
	if diagnostic.ResponseModel == "" || terminal {
		diagnostic.ResponseModel = model
	}
}

// The caller already needs the complete JSON body. Reuse those bytes instead
// of buffering a second copy or adding a reader/round trip for observation.
func readObservedUpstreamJSON(body io.ReadCloser) ([]byte, error) {
	payload, err := io.ReadAll(body)
	if traced := responseModelTraceBody(body); traced != nil && err == nil && !traced.captureError {
		traced.observer.observeResponseModel("", payload)
	}
	return payload, err
}

func (observer *TransportObserver) observeResponseModel(event string, payload []byte) {
	observer.update(func(*UpstreamTransportDiagnostic) {
		if declared := gjson.GetBytes(payload, "type").String(); declared != "" {
			event = declared
		}
		observeUpstreamResponseModel(observer.attempt, payload, event)
	})
}

// These wrappers observe bytes without modifying them. Do not unwrap protocol
// translators: their model fields may be synthesized from the request.
func responseModelTraceBody(body io.Reader) *tracedResponseBody {
	for {
		switch wrapped := body.(type) {
		case *tracedResponseBody:
			return wrapped
		case *encryptedErrorObserver:
			body = wrapped.ReadCloser
		case *codexTelemetryBody:
			body = wrapped.ReadCloser
		default:
			return nil
		}
	}
}
