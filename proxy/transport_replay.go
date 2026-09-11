package proxy

import (
	"context"
	"errors"
)

type payloadDeliveryError struct {
	cause error
}

type rejectedWebsocketRequestError struct {
	cause error
}

func (failure *rejectedWebsocketRequestError) Error() string { return failure.cause.Error() }

func (failure *rejectedWebsocketRequestError) Unwrap() error { return failure.cause }

func (failure *payloadDeliveryError) Error() string {
	return "上游请求是否完成无法确认，已停止自动重试。" + failure.cause.Error()
}

func (failure *payloadDeliveryError) Unwrap() error { return failure.cause }

func BlockTransportReplay(err error) error {
	if err == nil || TransportReplayBlocked(err) {
		return err
	}
	return &payloadDeliveryError{cause: err}
}

func TransportReplayBlocked(err error) bool {
	var failure *payloadDeliveryError
	return errors.As(err, &failure)
}

func ensureTransportTrace(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if upstreamTraceFromContext(ctx) == nil {
		ctx = context.WithValue(ctx, upstreamTraceContextKey{}, &upstreamTraceAudit{requestID: NewUpstreamSessionUUID()})
	}
	return ctx
}

func (observer *TransportObserver) TransportError(err error) error {
	if observer == nil || err == nil {
		return err
	}
	var rejected *rejectedWebsocketRequestError
	if errors.As(err, &rejected) && !TransportReplayBlocked(err) {
		return err
	}
	observer.audit.mu.Lock()
	blocked := !observer.attempt.idempotent && observer.attempt.transport.SendPhase != "before_payload"
	if blocked {
		observer.attempt.transport.ReplayBlocked = true
	}
	observer.audit.mu.Unlock()
	if blocked {
		return BlockTransportReplay(err)
	}
	return err
}
