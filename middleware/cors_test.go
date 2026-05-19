package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORS_AllowsCredentialsForConfiguredOrigin(t *testing.T) {
	t.Setenv("MODELHUB_CORS_ORIGIN", "http://localhost:3000")
	rr := performCORSRequest(t, http.MethodGet, "http://localhost:3000")

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow-credentials = %q", got)
	}
}

func TestCORS_DoesNotSendCredentialsWithWildcard(t *testing.T) {
	t.Setenv("MODELHUB_CORS_ORIGIN", "http://localhost:3000")
	rr := performCORSRequest(t, http.MethodGet, "http://evil.example")

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("allow-credentials = %q", got)
	}
}

func TestCORS_PreflightReturnsNoContent(t *testing.T) {
	t.Setenv("MODELHUB_CORS_ORIGIN", "http://localhost:3000")
	rr := performCORSRequest(t, http.MethodOptions, "http://localhost:3000")

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}
}

func performCORSRequest(t *testing.T, method string, origin string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(method, "/ok", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}
