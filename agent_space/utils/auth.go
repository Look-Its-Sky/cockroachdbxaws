package utils

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// AuthHeader is the header clients present. Not "Authorization", because that
// name invites confusion with the OpenRouter and CockroachDB credentials the
// server holds — this one is for callers of this API.
const AuthHeader = "X-Agent-Token"

// RequireToken gates the expensive and mutating routes behind a shared secret
// from API_TOKEN.
//
// The routes it protects cost real money — one /agent run is on the order of
// 15k tokens against a billable key, and even /retrieve embeds its query — and
// /store writes into the vector index while /retrieve reads incident data back
// out. A public URL without this is an open invitation to spend someone else's
// credits and read their data. Only /ping and /tools stay open: they are free,
// read-only, and they are the parts worth showing off.
//
// With API_TOKEN unset the middleware is a no-op, which keeps local development
// frictionless, and it says so loudly at boot rather than silently.
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

		// Constant time, so a caller cannot narrow the token down by timing
		// how long the comparison takes.
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or invalid " + AuthHeader,
			})
			return
		}

		c.Next()
	}
}
