package router

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func isRemovedSPA(path string) bool {
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	prefixes := []string{
		"/metric/explorer",
		"/object/explorer",
		"/metrics-built-in",
		"/log/explorer",
		"/log/index-patterns",
		"/dashboards",
		"/public-dashboards",
		"/embedded-dashboards",
		"/embedded-products",
		"/embedded-product",
		"/targets",
		"/recording-rules",
		"/ai-config",
		"/system/version",
		"/help/version",
		"/trace",
		"/overview",
		"/home",
		"/landing",
	}
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func isRemovedAPI(path string) bool {
	path = strings.TrimPrefix(path, "/api/n9e")
	switch {
	case strings.HasPrefix(path, "/targets"), path == "/target-update", strings.HasPrefix(path, "/target/"):
		return true
	case strings.Contains(path, "/boards"), strings.HasPrefix(path, "/board/"), path == "/boards":
		return true
	case strings.HasPrefix(path, "/share-charts"), strings.HasPrefix(path, "/dashboard-annotation"):
		return true
	case strings.Contains(path, "/recording-rule"):
		return true
	case strings.HasPrefix(path, "/embedded-"):
		return true
	case strings.HasPrefix(path, "/ai-llm"):
		return true
	case strings.HasPrefix(path, "/metric-views"), strings.HasPrefix(path, "/builtin-metric"):
		return true
	case strings.HasPrefix(path, "/agents/categraf"):
		return true
	default:
		return false
	}
}

func (rt *Router) blockRemovedAPIs() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isRemovedAPI(c.Request.URL.Path) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"err": "not found", "request_id": c.GetString("trace_id")})
			return
		}
		c.Next()
	}
}
