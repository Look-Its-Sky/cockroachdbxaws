package utils

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// the header clients present, not "Authorization", which invites confusion with the credentials the server holds
const AuthHeader = "X-Agent-Token"

// gates the expensive and mutating routes behind API_TOKEN; a no-op when unset so local dev stays frictionless
func RequireToken() gin.HandlerFunc {
	token := strings.TrimSpace(os.Getenv("API_TOKEN"))

	if token == "" {
		log.Println("API_TOKEN is not set: /store, /retrieve, /ask and /agent are " +
			"UNAUTHENTICATED. Set it before exposing this server publicly.")
		return func(c *gin.Context) { c.Next() }
	}

	log.Printf("API_TOKEN is set: /store and /agent require the %s header.", AuthHeader)

	return func(c *gin.Context) {
		presented := c.GetHeader(AuthHeader)
		if presented == "" {
			presented = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		}

		// constant time, so a caller cannot narrow the token down by timing
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or invalid " + AuthHeader,
			})
			return
		}

		c.Next()
	}
}
