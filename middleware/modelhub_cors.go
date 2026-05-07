// Modelhub-specific CORS — S11.
//
// The inherited CORS() middleware uses AllowAllOrigins=true together with
// AllowCredentials=true. That combination is invalid per the CORS spec
// (browsers reject Access-Control-Allow-Origin:* whenever credentials are
// included), and unsafe in any case. Modelhub's frontend at localhost:5173
// (dev) and the production domain need a strict, explicit allowlist.
//
// Configure via MODELHUB_CORS_ORIGIN — comma-separated list of allowed
// origins. Empty / unset falls back to the dev defaults so a fresh checkout
// works without env wrangling.

package middleware

import (
	"os"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// DefaultModelhubCORSOrigins is the dev fallback when MODELHUB_CORS_ORIGIN
// is unset. Picks the Vite dev server's default port plus localhost
// variants developers commonly hit.
var DefaultModelhubCORSOrigins = []string{
	"http://localhost:5173",
	"http://127.0.0.1:5173",
	"http://localhost:3000",
	"http://127.0.0.1:3000",
}

// ModelhubCORS returns a CORS middleware suited for the modelhub /v1 +
// /admin endpoints: explicit origin list, credentials enabled, no
// wildcards. Origins come from MODELHUB_CORS_ORIGIN (comma-separated)
// when set, otherwise the dev defaults.
func ModelhubCORS() gin.HandlerFunc {
	origins := readModelhubCORSOrigins()
	cfg := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With", "Cookie"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           60 * 60, // 1h preflight cache
	}
	return cors.New(cfg)
}

// readModelhubCORSOrigins parses the MODELHUB_CORS_ORIGIN env var, trimming
// whitespace and dropping empty entries. Returns the dev defaults when the
// env var is empty.
func readModelhubCORSOrigins() []string {
	raw := os.Getenv("MODELHUB_CORS_ORIGIN")
	if raw == "" {
		// Return a copy so mutation by a downstream caller doesn't leak.
		out := make([]string, len(DefaultModelhubCORSOrigins))
		copy(out, DefaultModelhubCORSOrigins)
		return out
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		out = append(out, DefaultModelhubCORSOrigins...)
	}
	return out
}
