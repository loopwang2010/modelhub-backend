// S3Store unit tests — exercise the production path with mocked AWS
// clients. No real R2 / S3 credentials needed.
//
// An optional integration test (TestS3Store_Integration) runs against
// a real R2 bucket when R2_INTEGRATION=1 is set in the environment.
// Default (no env var) skips it so CI doesn't need cloud creds.

package asset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ─────────────────────────────────────────────────────────────────────────
// Mocks
// ─────────────────────────────────────────────────────────────────────────

type fakeUploader struct {
	gotIn   *s3.PutObjectInput
	gotBody []byte
	err     error
}

func (f *fakeUploader) Upload(ctx context.Context, in *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	f.gotIn = in
	if in != nil && in.Body != nil {
		// Drain the body so the size counter matches what would have
		// shipped over the wire.
		buf, err := io.ReadAll(in.Body)
		if err != nil {
			return nil, err
		}
		f.gotBody = buf
	}
	if f.err != nil {
		return nil, f.err
	}
	return &manager.UploadOutput{
		Location: "https://example.r2/" + aws.ToString(in.Key),
	}, nil
}

type fakeHead struct {
	exists  bool
	gotKey  string
	headErr error
}

func (f *fakeHead) HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.gotKey = aws.ToString(in.Key)
	if f.headErr != nil {
		return nil, f.headErr
	}
	if !f.exists {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{}, nil
}

type fakePresigner struct {
	gotKey string
	gotIn  *s3.GetObjectInput
	url    string
	err    error
}

func (f *fakePresigner) PresignGetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	f.gotIn = in
	f.gotKey = aws.ToString(in.Key)
	if f.err != nil {
		return nil, f.err
	}
	url := f.url
	if url == "" {
		url = "https://signed.example.r2/" + f.gotKey + "?sig=mock"
	}
	return &v4.PresignedHTTPRequest{URL: url}, nil
}

// withMocks builds an S3Store directly bypassing NewS3Store, wiring in
// the supplied fakes. Used by all unit tests below.
func withMocks(bucket, publicBase string, up s3Uploader, head s3HeadAPI, pre s3PresignAPI) *S3Store {
	return &S3Store{
		bucket:        bucket,
		publicURLBase: publicBase,
		presignTTL:    10 * time.Minute,
		uploader:      up,
		head:          head,
		presigner:     pre,
	}
}

// ─────────────────────────────────────────────────────────────────────────
// NewS3Store config validation
// ─────────────────────────────────────────────────────────────────────────

func TestNewS3Store_RequiredFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  S3Config
	}{
		{"missing bucket", S3Config{AccessKeyID: "k", SecretAccessKey: "s", AccountID: "a"}},
		{"missing creds", S3Config{Bucket: "b", AccountID: "a"}},
		{"missing endpoint and account", S3Config{Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"public-url-base missing trailing slash", S3Config{
			Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", AccountID: "a",
			PublicURLBase: "https://cdn.example.com",
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewS3Store(tc.cfg)
			if !errors.Is(err, ErrStorageNotConfigured) {
				t.Errorf("want ErrStorageNotConfigured, got %v", err)
			}
		})
	}
}

func TestNewS3Store_DefaultRegion_Auto(t *testing.T) {
	t.Parallel()
	st, err := NewS3Store(S3Config{
		Bucket:          "b",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		AccountID:       "abc123",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if st == nil {
		t.Fatal("nil store")
	}
	if st.bucket != "b" {
		t.Errorf("bucket: want %q, got %q", "b", st.bucket)
	}
}

func TestNewS3Store_AcceptsExplicitEndpoint(t *testing.T) {
	t.Parallel()
	_, err := NewS3Store(S3Config{
		Bucket:          "b",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		Endpoint:        "https://minio.local:9000",
		PublicURLBase:   "https://cdn.example.com/",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Bare struct (not constructed via NewS3Store) — fail-fast contract
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_BareStruct_FailsFast(t *testing.T) {
	t.Parallel()
	s := &S3Store{}
	_, err := s.Put(context.Background(), "outputs/x/y.bin", "image/png", strings.NewReader("z"))
	if !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("Put: want ErrStorageNotConfigured, got %v", err)
	}
	if _, err := s.Exists(context.Background(), "outputs/x/y.bin"); !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("Exists: want ErrStorageNotConfigured, got %v", err)
	}
	if _, err := s.SignedURL(context.Background(), "outputs/x/y.bin", time.Minute); !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("SignedURL: want ErrStorageNotConfigured, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Put — public URL fast path
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_Put_PublicURL(t *testing.T) {
	t.Parallel()
	up := &fakeUploader{}
	head := &fakeHead{}
	pre := &fakePresigner{}
	st := withMocks("modelhub-outputs", "https://cdn.modelhub.com/", up, head, pre)

	body := bytes.NewReader([]byte("hello world"))
	res, err := st.Put(context.Background(), "outputs/task-1/abc.png", "image/png", body)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Bucket + key + content type passed through.
	if got := aws.ToString(up.gotIn.Bucket); got != "modelhub-outputs" {
		t.Errorf("bucket: want %q, got %q", "modelhub-outputs", got)
	}
	if got := aws.ToString(up.gotIn.Key); got != "outputs/task-1/abc.png" {
		t.Errorf("key: want %q, got %q", "outputs/task-1/abc.png", got)
	}
	if got := aws.ToString(up.gotIn.ContentType); got != "image/png" {
		t.Errorf("content-type: want %q, got %q", "image/png", got)
	}
	if string(up.gotBody) != "hello world" {
		t.Errorf("body: want %q, got %q", "hello world", string(up.gotBody))
	}

	// Result URL uses public base, not presigner.
	wantURL := "https://cdn.modelhub.com/outputs/task-1/abc.png"
	if res.URL != wantURL {
		t.Errorf("URL: want %q, got %q", wantURL, res.URL)
	}
	if res.SizeBytes != int64(len("hello world")) {
		t.Errorf("size: want %d, got %d", len("hello world"), res.SizeBytes)
	}
	if pre.gotKey != "" {
		t.Errorf("presigner unexpectedly called for public-URL path: %q", pre.gotKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Put — presigned URL fallback when no public base
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_Put_PresignedFallback(t *testing.T) {
	t.Parallel()
	up := &fakeUploader{}
	head := &fakeHead{}
	pre := &fakePresigner{url: "https://signed.example.r2/outputs/task-2/zzz.mp4?sig=abc"}
	st := withMocks("modelhub-outputs", "" /* no public base */, up, head, pre)

	res, err := st.Put(context.Background(), "outputs/task-2/zzz.mp4", "video/mp4", strings.NewReader("vid-bytes"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(res.URL, "https://signed.example.r2/") {
		t.Errorf("URL: want presigned URL, got %q", res.URL)
	}
	if pre.gotKey != "outputs/task-2/zzz.mp4" {
		t.Errorf("presigner key: want %q, got %q", "outputs/task-2/zzz.mp4", pre.gotKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Put — error wrapping
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_Put_WrapsUploaderError(t *testing.T) {
	t.Parallel()
	up := &fakeUploader{err: errors.New("upstream timeout")}
	st := withMocks("b", "https://cdn.example.com/", up, &fakeHead{}, &fakePresigner{})

	_, err := st.Put(context.Background(), "outputs/t/k.bin", "x", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "s3 upload") {
		t.Errorf("error message should mention 's3 upload', got %q", err.Error())
	}
}

func TestS3Store_Put_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	st := withMocks("b", "https://cdn.example.com/", &fakeUploader{}, &fakeHead{}, &fakePresigner{})

	if _, err := st.Put(context.Background(), "../escape", "x", strings.NewReader("z")); err == nil {
		t.Error("traversal key: expected error, got nil")
	}
	if _, err := st.Put(context.Background(), "outputs/t/k.bin", "x", nil); err == nil {
		t.Error("nil body: expected error, got nil")
	}
}

func TestS3Store_Put_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	up := &fakeUploader{}
	st := withMocks("b", "https://cdn.example.com/", up, &fakeHead{}, &fakePresigner{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Use io.Pipe so the read would block forever without ctx.
	pr, pw := io.Pipe()
	defer pw.Close()
	_, err := st.Put(ctx, "outputs/t/k.bin", "x", pr)
	if err == nil {
		t.Fatal("expected error from cancelled ctx")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Exists — three terminal cases
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_Exists_True(t *testing.T) {
	t.Parallel()
	head := &fakeHead{exists: true}
	st := withMocks("b", "", &fakeUploader{}, head, &fakePresigner{})

	ok, err := st.Exists(context.Background(), "outputs/t/k.bin")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("want true, got false")
	}
	if head.gotKey != "outputs/t/k.bin" {
		t.Errorf("key: want %q, got %q", "outputs/t/k.bin", head.gotKey)
	}
}

func TestS3Store_Exists_FalseOnNotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"types.NotFound", &types.NotFound{}},
		{"types.NoSuchKey", &types.NoSuchKey{}},
		{"smithy 404", &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 404}},
			Err:      errors.New("404 not found"),
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			head := &fakeHead{headErr: tc.err}
			st := withMocks("b", "", &fakeUploader{}, head, &fakePresigner{})

			ok, err := st.Exists(context.Background(), "outputs/t/k.bin")
			if err != nil {
				t.Fatalf("Exists: want nil error on 404, got %v", err)
			}
			if ok {
				t.Error("want false, got true")
			}
		})
	}
}

func TestS3Store_Exists_PropagatesOtherErrors(t *testing.T) {
	t.Parallel()
	head := &fakeHead{headErr: errors.New("internal server error")}
	st := withMocks("b", "", &fakeUploader{}, head, &fakePresigner{})

	_, err := st.Exists(context.Background(), "outputs/t/k.bin")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "s3 head") {
		t.Errorf("error should mention 's3 head', got %q", err.Error())
	}
}

func TestS3Store_Exists_RejectsBadKey(t *testing.T) {
	t.Parallel()
	st := withMocks("b", "", &fakeUploader{}, &fakeHead{}, &fakePresigner{})
	if _, err := st.Exists(context.Background(), "../escape"); err == nil {
		t.Fatal("expected error for traversal key")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// SignedURL
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_SignedURL(t *testing.T) {
	t.Parallel()
	pre := &fakePresigner{url: "https://signed.example.r2/outputs/t/k.bin?sig=xyz"}
	st := withMocks("modelhub-outputs", "", &fakeUploader{}, &fakeHead{}, pre)

	url, err := st.SignedURL(context.Background(), "outputs/t/k.bin", 5*time.Minute)
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	if !strings.HasPrefix(url, "https://signed.example.r2/") {
		t.Errorf("URL: want presigned, got %q", url)
	}
	if pre.gotKey != "outputs/t/k.bin" {
		t.Errorf("key: want %q, got %q", "outputs/t/k.bin", pre.gotKey)
	}
	if got := aws.ToString(pre.gotIn.Bucket); got != "modelhub-outputs" {
		t.Errorf("bucket: want %q, got %q", "modelhub-outputs", got)
	}
}

func TestS3Store_SignedURL_DefaultsTTL(t *testing.T) {
	t.Parallel()
	pre := &fakePresigner{}
	st := withMocks("b", "", &fakeUploader{}, &fakeHead{}, pre)

	if _, err := st.SignedURL(context.Background(), "outputs/t/k.bin", 0); err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
}

func TestS3Store_SignedURL_WrapsError(t *testing.T) {
	t.Parallel()
	pre := &fakePresigner{err: errors.New("presign failed")}
	st := withMocks("b", "", &fakeUploader{}, &fakeHead{}, pre)

	_, err := st.SignedURL(context.Background(), "outputs/t/k.bin", time.Minute)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "presign") {
		t.Errorf("error should mention 'presign', got %q", err.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Smoke test on Storage interface satisfaction
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_SatisfiesStorage(t *testing.T) {
	t.Parallel()
	var _ Storage = (*S3Store)(nil)
}

// ─────────────────────────────────────────────────────────────────────────
// Optional integration test against real R2
// ─────────────────────────────────────────────────────────────────────────

func TestS3Store_Integration(t *testing.T) {
	if os.Getenv("R2_INTEGRATION") != "1" {
		t.Skip("skipping integration test; set R2_INTEGRATION=1 with R2_ACCOUNT_ID/R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_BUCKET to enable")
	}
	cfg := S3Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		Endpoint:        os.Getenv("R2_ENDPOINT"),
		Bucket:          os.Getenv("R2_BUCKET"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		PublicURLBase:   os.Getenv("R2_PUBLIC_URL_BASE"),
	}
	st, err := NewS3Store(cfg)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	key := fmt.Sprintf("outputs/t7-integration/%d.txt", time.Now().UnixNano())
	body := strings.NewReader("integration-test " + time.Now().Format(time.RFC3339))
	res, err := st.Put(context.Background(), key, "text/plain", body)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Logf("uploaded: key=%s url=%s size=%d", res.Key, res.URL, res.SizeBytes)
	exists, err := st.Exists(context.Background(), key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Errorf("Exists returned false for just-uploaded key %q", key)
	}
}

