package proxy

import (
	"time"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

type ResponsesStreamDeliveryDiagnostic struct {
	TerminalEvent      string `json:"terminal_event,omitempty"`
	ResponseStatus     string `json:"response_status,omitempty"`
	IncompleteReason   string `json:"incomplete_reason,omitempty"`
	TerminalReceivedAt int64  `json:"terminal_received_at_unix_ms,omitempty"`
	CancelObservedAt   int64  `json:"cancel_observed_at_unix_ms,omitempty"`
	UsageSource        string `json:"usage_source"`
	UsageAfterCancel   bool   `json:"usage_received_after_cancel"`
	TerminalWrite      string `json:"terminal_write"`
	TerminalWriteAt    int64  `json:"terminal_write_at_unix_ms,omitempty"`
	DownstreamStatus   string `json:"downstream_status"`
	WriteError         string `json:"write_error,omitempty"`
}

func (observer *TransportObserver) ResponsesTerminal(eventType string, payload []byte, canceled bool) {
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.error", "response.canceled", "response.cancelled", "error":
	default:
		return
	}
	delivery := ResponsesStreamDeliveryDiagnostic{
		TerminalEvent:      eventType,
		ResponseStatus:     safeDiagnosticToken(gjson.GetBytes(payload, "response.status").String()),
		IncompleteReason:   safeDiagnosticToken(gjson.GetBytes(payload, "response.incomplete_details.reason").String()),
		TerminalReceivedAt: time.Now().UnixMilli(),
		UsageSource:        "missing", TerminalWrite: "not_attempted", DownstreamStatus: "pending",
	}
	switch delivery.ResponseStatus {
	case "", "completed", "incomplete", "failed", "canceled", "cancelled":
	default:
		delivery.ResponseStatus = "unknown"
	}
	switch delivery.IncompleteReason {
	case "", "max_output_tokens", "content_filter":
	default:
		delivery.IncompleteReason = "other"
	}
	if gjson.GetBytes(payload, "response.usage").IsObject() {
		delivery.UsageSource = "upstream"
		delivery.UsageAfterCancel = canceled
	}
	if canceled {
		delivery.CancelObservedAt = time.Now().UnixMilli()
	}
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) { diagnostic.StreamDelivery = &delivery })
}

func (observer *TransportObserver) ResponsesTerminalWrite(disposition string, err error) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		if diagnostic.StreamDelivery == nil {
			return
		}
		delivery := *diagnostic.StreamDelivery
		delivery.TerminalWrite = disposition
		delivery.TerminalWriteAt = time.Now().UnixMilli()
		if err != nil {
			delivery.TerminalWrite = "failed"
			delivery.WriteError = security.SafeTruncate(security.MaskSensitiveData(err.Error()), 256)
		}
		diagnostic.StreamDelivery = &delivery
	})
}

func (observer *TransportObserver) ResponsesDeliveryFinished(ctxErr, writeErr error) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		delivery := ResponsesStreamDeliveryDiagnostic{UsageSource: "missing", TerminalWrite: "not_seen"}
		if diagnostic.StreamDelivery != nil {
			delivery = *diagnostic.StreamDelivery
		}
		switch {
		case writeErr != nil:
			delivery.DownstreamStatus = "write_failed"
			delivery.WriteError = security.SafeTruncate(security.MaskSensitiveData(writeErr.Error()), 256)
		case ctxErr != nil:
			delivery.DownstreamStatus = "client_canceled"
		case delivery.TerminalWrite == "accepted":
			delivery.DownstreamStatus = "write_accepted"
		case delivery.TerminalWrite == "buffered":
			delivery.DownstreamStatus = "buffered"
		default:
			delivery.DownstreamStatus = "terminal_not_written"
		}
		if ctxErr != nil && delivery.CancelObservedAt == 0 {
			delivery.CancelObservedAt = time.Now().UnixMilli()
		}
		diagnostic.StreamDelivery = &delivery
	})
}
