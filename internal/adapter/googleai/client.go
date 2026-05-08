// Package googleai — Vertex AI HTTP client wiring.
//
// The client is intentionally thin: build URLs, attach the OAuth2 transport,
// honour caller-supplied context. Provider-specific endpoint shapes are the
// only Google-aware bit and they live in this package only (per AP-1 and
// ADR-018). The rest of the codebase NEVER imports anything Vertex-specific.

package googleai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// LocationEnvVar lets operators override the deployment region (default
// "us-central1"). Veo3 is GA in us-central1 and a few EU regions; mainland
// China and Hong Kong regions are blocked at submit time per TOS-RESEARCH §2.
const LocationEnvVar = "GOOGLE_VERTEX_LOCATION"

// DefaultLocation is the region used when GOOGLE_VERTEX_LOCATION is unset.
const DefaultLocation = "us-central1"

// blockedLocations is the set of regions where modelhub MUST NOT submit
// Veo3 jobs even if the operator has accidentally configured them. Any
// mainland-China or Hong Kong location is blocked at the submit boundary;
// see TOS-RESEARCH.md §2.
var blockedLocations = map[string]struct{}{
	// Mainland China regions (none are GA for Vertex AI Veo3 anyway, but
	// the explicit allow-list / deny-list makes the intent reviewable).
	"asia-east2":     {}, // Hong Kong
	"asia-northeast3": {}, // ineligible per geo policy doc
	"cn-north-1":     {}, // hypothetical mainland-China zone
	"cn-east-1":      {},
}

// config bundles the runtime knobs for the adapter. All fields are read
// once at construction; the resulting *adapter is otherwise immutable.
type config struct {
	location   string
	httpClient *http.Client
	credSource credentialSource

	// baseURLOverride lets tests redirect API traffic to httptest. Empty in
	// production; uses the standard {LOCATION}-aiplatform.googleapis.com host.
	baseURLOverride string
}

// loadConfig builds a config from process env. Returns adapter.ErrNotConfigured
// when GOOGLE_APPLICATION_CREDENTIALS is missing or unreadable.
func loadConfig(ctx context.Context, envLookup func(string) string) (*config, error) {
	if envLookup == nil {
		envLookup = func(k string) string { return "" }
	}
	credPath := envLookup(CredentialsEnvVar)
	if strings.TrimSpace(credPath) == "" {
		return nil, fmt.Errorf("googleai: %w: %s unset", adapter.ErrNotConfigured, CredentialsEnvVar)
	}
	location := strings.TrimSpace(envLookup(LocationEnvVar))
	if location == "" {
		location = DefaultLocation
	}
	if isBlockedLocation(location) {
		return nil, fmt.Errorf("googleai: %w: location %q blocked by geo policy (see TOS §2)",
			adapter.ErrInvalidParams, location)
	}
	src := newFileCredentialSource(credPath)
	httpClient, err := authedClient(ctx, src, nil)
	if err != nil {
		return nil, err
	}
	return &config{
		location:   location,
		httpClient: httpClient,
		credSource: src,
	}, nil
}

// isBlockedLocation reports whether the operator-configured location is on
// the geo deny-list. Comparison is case-insensitive.
func isBlockedLocation(loc string) bool {
	_, blocked := blockedLocations[strings.ToLower(strings.TrimSpace(loc))]
	return blocked
}

// baseURL returns the API host for the configured location. Tests can
// override this entirely via baseURLOverride.
func (c *config) baseURL() string {
	if c.baseURLOverride != "" {
		return strings.TrimRight(c.baseURLOverride, "/")
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com", c.location)
}

// submitURL returns the predictLongRunning POST URL for a given upstream
// model id (e.g., "veo-3.0-generate-preview"). The full op name returned by
// Submit will reuse this same project/location/model triple.
func (c *config) submitURL(upstreamModel string) (string, error) {
	if upstreamModel == "" {
		return "", errors.New("googleai: upstream model id required")
	}
	project := c.credSource.projectID()
	if project == "" {
		return "", fmt.Errorf("googleai: %w: project id unavailable", adapter.ErrNotConfigured)
	}
	return fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predictLongRunning",
		c.baseURL(), project, c.location, upstreamModel,
	), nil
}

// pollURL returns the GET-operation URL for a previously-issued operation
// name. Per Google's API, the operation name itself includes
// projects/.../locations/.../publishers/google/models/.../operations/...
// so we just need the host/version prefix.
func (c *config) pollURL(opName string) (string, error) {
	if opName == "" {
		return "", errors.New("googleai: empty operation name")
	}
	return fmt.Sprintf("%s/v1/%s", c.baseURL(), opName), nil
}

// cancelURL returns the :cancel POST URL for an operation.
func (c *config) cancelURL(opName string) (string, error) {
	if opName == "" {
		return "", errors.New("googleai: empty operation name")
	}
	return fmt.Sprintf("%s/v1/%s:cancel", c.baseURL(), opName), nil
}
