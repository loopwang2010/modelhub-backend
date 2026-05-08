// S3Store — production-grade Storage implementation backed by an
// S3-compatible object store. The default deployment target is
// Cloudflare R2 (zero egress fees + S3 API compatibility), but the
// same client works against AWS S3, MinIO, and any other S3-API
// service that supports path-style addressing.
//
// AP-13 streaming guarantee: uploads use s3manager.Uploader, which
// chunks the io.Reader into 5 MiB parts and dispatches them as the
// reader produces bytes. The full payload is never buffered in memory.
//
// R2-specific quirks handled by NewS3Store:
//
//   - Region is hard-coded to "auto" — R2 does not use AWS regions.
//   - Endpoint is the per-account host
//     (https://{account_id}.r2.cloudflarestorage.com).
//   - Path-style addressing is forced (UsePathStyle=true) — virtual-
//     hosted style breaks for buckets whose names contain dots or
//     don't match R2's hostname conventions.
//
// Mockable surface: the production constructor wires the concrete
// AWS SDK clients into three small interfaces (s3Uploader, s3HeadAPI,
// s3PresignAPI). Unit tests inject fakes for these — no real R2
// connectivity required to exercise the package.

package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// DefaultPresignTTL is the lifetime of a presigned GET URL when the
// caller doesn't specify one.
const DefaultPresignTTL = 15 * time.Minute

// ─────────────────────────────────────────────────────────────────────────
// Mockable interfaces (kept tight — only the methods we actually call)
// ─────────────────────────────────────────────────────────────────────────

// s3Uploader is satisfied by *manager.Uploader. Wraps streaming PutObject.
type s3Uploader interface {
	Upload(ctx context.Context, in *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

// s3HeadAPI is satisfied by *s3.Client. Used for Exists.
type s3HeadAPI interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// s3PresignAPI is satisfied by *s3.PresignClient. Used for short-lived
// signed GET URLs when no public URL prefix is configured.
type s3PresignAPI interface {
	PresignGetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// ─────────────────────────────────────────────────────────────────────────
// S3Config + constructor
// ─────────────────────────────────────────────────────────────────────────

// S3Config carries the wire-up parameters for an R2/S3 client.
//
// All credentials come from env vars in the calling layer (see
// docs/DEPLOY.md). This struct never reads the environment itself —
// keeping it env-agnostic makes it trivial to build configurations
// for tests.
type S3Config struct {
	// AccountID is the Cloudflare R2 account ID. When set and Endpoint
	// is empty, the constructor synthesizes the standard R2 endpoint
	// (https://{AccountID}.r2.cloudflarestorage.com). Optional for
	// non-R2 deployments — supply Endpoint directly instead.
	AccountID string

	// Endpoint is the explicit S3 API endpoint. Overrides the synthesized
	// R2 endpoint. Required for non-R2 services (MinIO, AWS S3 with
	// custom endpoint). Must include scheme.
	Endpoint string

	// Region is the AWS-style region. For R2, MUST be "auto".
	// Defaults to "auto" when empty.
	Region string

	// Bucket is the bucket name. Required.
	Bucket string

	// AccessKeyID + SecretAccessKey are the static credentials. Both
	// required. The caller is responsible for sourcing these from a
	// secret manager (env vars, Vault, etc.).
	AccessKeyID     string
	SecretAccessKey string

	// PublicURLBase is the optional fast-path public URL. If set, Put
	// returns "PublicURLBase + key" as the asset URL — used when the
	// bucket is fronted by a custom domain (R2's "Public Bucket" feature
	// or a Cloudflare Worker). When empty, Put returns a presigned URL
	// instead. MUST end with `/` when set.
	PublicURLBase string

	// PresignTTL is the lifetime of presigned URLs returned when
	// PublicURLBase is empty. 0 → DefaultPresignTTL.
	PresignTTL time.Duration

	// UsePathStyle forces path-style addressing. R2 + MinIO require
	// true. Defaults to true (path-style is the safer default).
	UsePathStyle *bool
}

// S3Store is the production Storage implementation backed by an
// S3-compatible object store (default: Cloudflare R2).
//
// Construct via NewS3Store. The bare struct is intentionally not
// usable — Put with a nil uploader returns ErrStorageNotConfigured to
// fail fast on misconfiguration.
type S3Store struct {
	bucket        string
	publicURLBase string // empty → use presigned URLs
	presignTTL    time.Duration

	uploader  s3Uploader
	head      s3HeadAPI
	presigner s3PresignAPI
}

// NewS3Store constructs a production S3Store from an S3Config. Returns
// ErrStorageNotConfigured wrapped with the missing field when required
// configuration is absent.
func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("asset: S3Config.Bucket required: %w", ErrStorageNotConfigured)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("asset: S3Config credentials required: %w", ErrStorageNotConfigured)
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("asset: S3Config requires Endpoint or AccountID: %w", ErrStorageNotConfigured)
		}
		endpoint = "https://" + cfg.AccountID + ".r2.cloudflarestorage.com"
	}
	if cfg.PublicURLBase != "" && !strings.HasSuffix(cfg.PublicURLBase, "/") {
		return nil, fmt.Errorf("asset: PublicURLBase must end with '/': %w", ErrStorageNotConfigured)
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	usePathStyle := true
	if cfg.UsePathStyle != nil {
		usePathStyle = *cfg.UsePathStyle
	}

	awsCfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = usePathStyle
		o.BaseEndpoint = aws.String(endpoint)
	})

	uploader := manager.NewUploader(client)
	presigner := s3.NewPresignClient(client)

	ttl := cfg.PresignTTL
	if ttl <= 0 {
		ttl = DefaultPresignTTL
	}

	return &S3Store{
		bucket:        cfg.Bucket,
		publicURLBase: cfg.PublicURLBase,
		presignTTL:    ttl,
		uploader:      uploader,
		head:          client,
		presigner:     presigner,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Storage interface impl
// ─────────────────────────────────────────────────────────────────────────

// Put implements Storage. Streams body to the configured bucket under
// key, returning either a public-URL-prefixed URL (fast path) or a
// presigned GET URL (when no public URL base is configured).
//
// AP-13 streaming: the body is wired straight into manager.Uploader,
// which chunks at 5 MiB intervals; the full payload is never buffered
// in process memory.
func (s *S3Store) Put(ctx context.Context, key string, contentType string, body io.Reader) (*PutResult, error) {
	if s == nil || s.uploader == nil {
		return nil, fmt.Errorf("asset: S3Store not constructed via NewS3Store: %w", ErrStorageNotConfigured)
	}
	if err := validateObjectKey(key); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("asset: nil body")
	}

	// Wrap body in a counter so we can return the byte count even
	// though the SDK doesn't surface it directly on the upload output.
	counter := &countingReader{r: ctxReader(ctx, body)}

	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   counter,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}

	if _, err := s.uploader.Upload(ctx, in); err != nil {
		return nil, fmt.Errorf("asset: s3 upload: %w", wrapAWSError(err))
	}

	url, err := s.urlFor(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("asset: build url after upload: %w", err)
	}

	return &PutResult{
		Key:       key,
		URL:       url,
		SizeBytes: counter.n,
	}, nil
}

// Exists reports whether key already exists in the bucket. A clean
// "not found" returns (false, nil); other errors are surfaced for the
// caller to handle.
//
// Used by idempotent retries — callers can skip a re-upload when the
// object is already present from a previous attempt.
func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	if s == nil || s.head == nil {
		return false, fmt.Errorf("asset: S3Store not constructed via NewS3Store: %w", ErrStorageNotConfigured)
	}
	if err := validateObjectKey(key); err != nil {
		return false, err
	}
	_, err := s.head.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("asset: s3 head: %w", wrapAWSError(err))
}

// SignedURL returns a presigned GET URL valid for ttl. Useful when
// the bucket is private and the caller wants to hand a short-lived
// download link to a client.
func (s *S3Store) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s == nil || s.presigner == nil {
		return "", fmt.Errorf("asset: S3Store not constructed via NewS3Store: %w", ErrStorageNotConfigured)
	}
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("asset: presign: %w", wrapAWSError(err))
	}
	return req.URL, nil
}

// urlFor returns the URL form Put advertises after a successful upload.
// Public URL when publicURLBase is set; presigned URL otherwise.
func (s *S3Store) urlFor(ctx context.Context, key string) (string, error) {
	if s.publicURLBase != "" {
		return s.publicURLBase + key, nil
	}
	return s.SignedURL(ctx, key, s.presignTTL)
}

// ─────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────

// countingReader wraps io.Reader and counts bytes read. The s3 manager
// drains the reader into chunks; we count after each Read so we have
// the authoritative size to return to the caller.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// isNotFound reports whether err represents an S3 404 / NoSuchKey.
// Two cases: the typed *types.NoSuchKey (thrown by GetObject) and the
// generic 404 returned by HeadObject (which uses *types.NotFound).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	// Fallback: smithy HTTP status. HeadObject returns a 404 wrapped in
	// a generic ResponseError without a typed NotFound on some endpoints.
	var hre *smithyhttp.ResponseError
	if errors.As(err, &hre) && hre.HTTPStatusCode() == 404 {
		return true
	}
	return false
}

// wrapAWSError replaces the raw AWS error chain with a string message
// so callers (and logs) don't get internal SDK type names. The
// underlying error is preserved via %w for unwrap.
func wrapAWSError(err error) error {
	if err == nil {
		return nil
	}
	var hre *smithyhttp.ResponseError
	if errors.As(err, &hre) {
		return fmt.Errorf("s3 http %d: %w", hre.HTTPStatusCode(), err)
	}
	return err
}
