package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadRequest_ValidateOK(t *testing.T) {
	r := UploadRequest{ContentType: "image/png", SizeBytes: 1024}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadRequest_RejectsEmptyContentType(t *testing.T) {
	r := UploadRequest{SizeBytes: 1024}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error")
	}
}

func TestUploadRequest_RejectsDisallowedContentType(t *testing.T) {
	r := UploadRequest{ContentType: "image/svg+xml", SizeBytes: 1024}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for SVG")
	}
}

func TestUploadRequest_RejectsZeroSize(t *testing.T) {
	r := UploadRequest{ContentType: "image/png", SizeBytes: 0}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for zero size")
	}
}

func TestUploadRequest_RejectsNegativeSize(t *testing.T) {
	r := UploadRequest{ContentType: "image/png", SizeBytes: -1}
	if err := r.Validate(); err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestUploadRequest_RejectsOversizeAP17(t *testing.T) {
	r := UploadRequest{ContentType: "image/png", SizeBytes: MaxUploadBytes + 1}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected AP-17 violation")
	}
	if !strings.Contains(err.Error(), "AP-17") {
		t.Errorf("error %q does not mention AP-17", err.Error())
	}
}

func TestUploadRequest_AllowsAllAllowedTypes(t *testing.T) {
	for ct := range allowedUploadContentTypes {
		r := UploadRequest{ContentType: ct, SizeBytes: 1024}
		if err := r.Validate(); err != nil {
			t.Errorf("%s rejected: %v", ct, err)
		}
	}
}

func TestUploadRequest_CaseInsensitiveContentType(t *testing.T) {
	r := UploadRequest{ContentType: "Image/PNG", SizeBytes: 1024}
	if err := r.Validate(); err != nil {
		t.Errorf("case-insensitive check failed: %v", err)
	}
}

func TestCreateUpload_HappyPath(t *testing.T) {
	body := `{"content_type":"image/png","size_bytes":2048,"filename":"x.png"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(body))
	rr := httptest.NewRecorder()
	CreateUpload().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var resp UploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.UploadID, "upload_") {
		t.Errorf("UploadID = %q", resp.UploadID)
	}
	if !strings.HasPrefix(resp.UploadURL, "https://cdn.modelhub.local/uploads/") {
		t.Errorf("UploadURL = %q (AP-19 violation?)", resp.UploadURL)
	}
	if resp.Method != "PUT" {
		t.Errorf("Method = %q", resp.Method)
	}
	if resp.Headers["Content-Type"] != "image/png" {
		t.Errorf("CT header = %q", resp.Headers["Content-Type"])
	}
	if resp.Headers["Content-Length"] != "2048" {
		t.Errorf("CL header = %q", resp.Headers["Content-Length"])
	}
}

func TestCreateUpload_RejectsGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil)
	rr := httptest.NewRecorder()
	CreateUpload().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestCreateUpload_RejectsBadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()
	CreateUpload().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestCreateUpload_RejectsValidationFailure(t *testing.T) {
	body := `{"content_type":"text/html","size_bytes":1}`
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(body))
	rr := httptest.NewRecorder()
	CreateUpload().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestCreateUpload_RejectsUnknownFields(t *testing.T) {
	body := `{"content_type":"image/png","size_bytes":1,"sneaky":"data"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(body))
	rr := httptest.NewRecorder()
	CreateUpload().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (DisallowUnknownFields)", rr.Code)
	}
}

func TestUploadID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := mintUploadID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
}
