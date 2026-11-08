package middleware

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/url"
	"strings"
)

// JSON-only writes plus an origin check protect session-authenticated admin
// actions. Permission and fresh user authentication remain separate gates.
func CatalogMaintenanceWrite() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		origin := ctx.GetHeader("Origin")
		cross := ctx.GetHeader("Sec-Fetch-Site") == "cross-site"
		if origin != "" {
			u, err := url.Parse(origin)
			cross = cross || err != nil || u.Host != ctx.Request.Host || (u.Scheme != "http" && u.Scheme != "https")
		}
		if cross || !strings.HasPrefix(strings.ToLower(ctx.GetHeader("Content-Type")), "application/json") {
			ctx.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "message": "Same-origin JSON request required"})
			return
		}
		ctx.Next()
	}
}
