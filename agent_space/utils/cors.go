package utils

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// how long a browser may cache a preflight, in seconds. A day: the route set
// does not change while the server is up.
const preflightMaxAge = "86400"

// lets a frontend on another origin call this API, since :5173 and :8080 are
// different origins. CORS_ORIGINS narrows it; unset means any, which is wrong
// the moment this is authenticated or exposed.
func CORS() gin.HandlerFunc {
	allowed := splitList(EnvOr("CORS_ORIGINS", ""))

	if len(allowed) == 0 {
		log.Println("CORS: any origin may call this API. Set CORS_ORIGINS to a comma-separated " +
			"list before exposing this server publicly.")
	} else {
		log.Printf("CORS: allowing %s", strings.Join(allowed, ", "))
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		// not a cross-origin request at all, so there is nothing to say about it
		if origin == "" {
			c.Next()
			return
		}

		if permitted(allowed, origin) {
			c.Header("Access-Control-Allow-Origin", origin)
			// the response varies by origin, so a shared cache must not serve
			// one origin's response to another
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type, "+AuthHeader+", Authorization")
			c.Header("Access-Control-Max-Age", preflightMaxAge)
		}

		// a preflight is answered here and never reaches a handler: it carries
		// no body and OPTIONS is not a route any of them serve
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// an empty list means every origin, matching the log line above
func permitted(allowed []string, origin string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

func splitList(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
