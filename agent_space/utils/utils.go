package utils

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

func Ping(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "pong",
	})
}

// RedactURL strips the password from a connection string so the target can be
// logged. Knowing which host is being dialled turns an opaque "connection
// refused" into an obvious misconfiguration.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable connection string)"
	}
	return u.Redacted()
}
