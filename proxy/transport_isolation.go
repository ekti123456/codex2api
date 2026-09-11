package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
)

type transportOwnerContextKey struct{}
type transportUserContextKey struct{}
type downstreamConnectionContextKey struct{}

func bindTransportOwner(ctx *gin.Context, policy verifiedNewAPIPolicyContext, verified bool) {
	owner := ""
	user := ""
	if verified && policy.MetaVerified && strings.TrimSpace(policy.Identity.UserID) != "" {
		encoded, _ := json.Marshal([]any{newAPIRuntimeScopeForPolicyContext(policy), policy.Identity.UserID, policy.Meta.TokenID})
		owner = hashRiskIdentity(string(encoded))
		encoded, _ = json.Marshal([]string{newAPIRuntimeScope(policy.APIKeyID, policy.Platform), policy.Identity.UserID})
		user = hashRiskIdentity(string(encoded))
	}
	requestContext := context.WithValue(ctx.Request.Context(), transportOwnerContextKey{}, owner)
	ctx.Request = ctx.Request.WithContext(context.WithValue(requestContext, transportUserContextKey{}, user))
}

func verifiedTransportUser(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	user, _ := ctx.Value(transportUserContextKey{}).(string)
	return user
}

func responseCacheOwnerForRequest(ctx *gin.Context, apiKeyID int64) string {
	if user := verifiedTransportUser(ctx.Request.Context()); user != "" {
		return responseCacheOwner(apiKeyID) + ":user:" + user
	}
	if apiKeyID > 0 {
		return responseCacheOwner(apiKeyID)
	}
	if credential := downstreamAuthorizationHeader(ctx.Request); credential != "" {
		return "credential:" + hashRiskIdentity(credential)
	}
	if connectionID := DownstreamWebsocketConnectionID(ctx.Request.Context()); connectionID != "" {
		return "anonymous-connection:" + connectionID
	}
	const key = "response-cache-anonymous-owner"
	if owner := ctx.GetString(key); owner != "" {
		return owner
	}
	owner := "anonymous:" + NewUpstreamSessionUUID()
	ctx.Set(key, owner)
	return owner
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
