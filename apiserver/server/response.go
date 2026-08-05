package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage"`
}

func requestIDFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get("request_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return c.GetHeader("X-Request-ID")
}

func (s *APIServer) RespondOK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{
		"request_id": requestIDFromGin(c),
		"data":       data,
		"error":      nil,
		"code":       0,
	})
}

func (s *APIServer) RespondHTTPError(c *gin.Context, status int, code string, message string) {
	if code == "" {
		code = ErrCodeTaskInvalidArgument
	}
	ec, ok := LookupErrorCode(code)
	if !ok {
		ec = ErrorCode{Code: code, Stage: "api", Retryable: false, HTTPStatus: status, Message: message}
	}
	if message == "" {
		message = ec.Message
	}
	c.JSON(status, gin.H{
		"request_id": requestIDFromGin(c),
		"data":       nil,
		"error":      APIError{Code: ec.Code, Message: message, Retryable: ec.Retryable, Stage: ec.Stage},
		"code":       status,
		"message":    message,
	})
}
