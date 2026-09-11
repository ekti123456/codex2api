package proxy

import (
	"errors"
	"fmt"

	"github.com/gorilla/websocket"
)

func (observer *TransportObserver) WebsocketReadFailure(err error, responseStarted, connectionLocal bool) error {
	if err == nil {
		return nil
	}
	code, source := 0, ""
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		code = closeErr.Code
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		code, source = websocket.CloseMessageTooBig, "local_read_limit"
	} else if code == websocket.CloseMessageTooBig {
		source = "peer_close"
	}
	rejected := source == "peer_close" && !responseStarted && !connectionLocal && !TransportReplayBlocked(err)
	observer.Failure("transport", "ws_read", code)
	bodySize := 0
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.MessageTooBigSource = source
		diagnostic.ReplayBlocked = !rejected
		diagnostic.ReplayDecision = "blocked_ambiguous_delivery"
		switch {
		case rejected:
			diagnostic.ReplayDecision = "peer_rejected_before_response"
		case connectionLocal:
			diagnostic.ReplayDecision = "blocked_connection_local_continuation"
		case source == "local_read_limit":
			diagnostic.ReplayDecision = "blocked_local_receive_limit"
		case source == "peer_close" && responseStarted:
			diagnostic.ReplayDecision = "blocked_response_already_started"
		}
		if diagnostic.ResponsesInput != nil {
			bodySize = diagnostic.ResponsesInput.JSONBytes
		}
	})
	if source != "" {
		globalWSSizeRouter.RecordMessageTooBig(bodySize)
		learned := bodySize >= wsSizeRouterMinSample && globalWSSizeRouter.PreferHTTP(bodySize)
		observer.update(func(diagnostic *UpstreamTransportDiagnostic) { diagnostic.HTTPSizeRouteLearned = learned })
	}
	wrapped := fmt.Errorf("websocket read error: %w", err)
	if rejected {
		return &rejectedWebsocketRequestError{cause: wrapped}
	}
	return BlockTransportReplay(wrapped)
}
