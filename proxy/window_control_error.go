package proxy

import (
	"net/http"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

func writeWindowControlError(request *gin.Context, status int, code, message string) {
	errorType := api.ErrorTypeInvalidRequest
	if status >= http.StatusInternalServerError {
		errorType = api.ErrorTypeServer
	} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
		errorType = api.ErrorTypeAuthentication
	}
	failure := api.NewAPIError(api.ErrorCode(code), message, errorType)
	api.ObserveError(request, status, failure)
	request.JSON(status, gin.H{"message": message, "code": code, "type": errorType})
}
