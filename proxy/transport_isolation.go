package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
)

type transportOwnerContextKey struct{}
type downstreamConnectionContextKey struct{}

func bindTransportOwner(ctx *gin.Context, policy verifiedNewAPIPolicyContext, verified bool) {
	owner := ""
	if verified && policy.MetaVerified && strings.TrimSpace(policy.Identity.UserID) != "" {
		encoded, _ := json.Marshal([]any{newAPIRuntimeScopeForPolicyContext(policy), policy.Identity.UserID, policy.Meta.TokenID})
		owner = hashRiskIdentity(string(encoded))
	}
	ctx.Request = ctx.Request.WithContext(context.WithValue(ctx.Request.Context(), transportOwnerContextKey{}, owner))
}

func WithDownstreamWebsocketConnection(ctx context.Context) context.Context {
	return context.WithValue(ctx, downstreamConnectionContextKey{}, NewUpstreamSessionUUID())
}

func DownstreamWebsocketConnectionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(downstreamConnectionContextKey{}).(string)
	return value
}

func WebsocketTransportOwner(ctx context.Context, apiKey string) string {
	owner := ""
	if ctx != nil {
		owner, _ = ctx.Value(transportOwnerContextKey{}).(string)
	}
	if owner == "" && strings.TrimSpace(apiKey) == "" {
		return "anonymous-" + NewUpstreamSessionUUID()
	}
	encoded, _ := json.Marshal([]string{"ws-owner-v1", apiKey, owner})
	return hashRiskIdentity(string(encoded))
}

func WebsocketTransportPartition(owner, connectionID, lane string) string {
	encoded, _ := json.Marshal([]string{"ws-partition-v1", owner, connectionID, lane})
	return "ws-isolated-" + hashRiskIdentity(string(encoded))
}
