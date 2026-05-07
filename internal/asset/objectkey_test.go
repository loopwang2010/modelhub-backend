package asset

import (
	"strings"
	"testing"
)

func TestBuildObjectKey_Deterministic(t *testing.T) {
	t.Parallel()
	k1 := BuildObjectKey("gen_abc", "https://example.com/x.png", "image/png")
	k2 := BuildObjectKey("gen_abc", "https://example.com/x.png", "image/png")
	if k1 != k2 {
		t.Errorf("not deterministic: %q vs %q", k1, k2)
	}
}

func TestBuildObjectKey_Layout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		taskID, url, mime, wantPrefix, wantExt string
	}{
		{"gen_abc", "https://x/y.png", "image/png", "outputs/gen_abc/", ".png"},
		{"gen_xyz", "gs://b/o.mp4", "video/mp4", "outputs/gen_xyz/", ".mp4"},
		{"gen_q", "https://x", "audio/wav", "outputs/gen_q/", ".wav"},
		{"gen_unk", "x", "weird/unknown", "outputs/gen_unk/", ".bin"},
		{"gen_p", "x", "image/png; charset=binary", "outputs/gen_p/", ".png"},
	}
	for _, tc := range cases {
		k := BuildObjectKey(tc.taskID, tc.url, tc.mime)
		if !strings.HasPrefix(k, tc.wantPrefix) {
			t.Errorf("prefix %q: got %q", tc.wantPrefix, k)
		}
		if !strings.HasSuffix(k, tc.wantExt) {
			t.Errorf("ext %q: got %q", tc.wantExt, k)
		}
	}
}

func TestBuildObjectKey_DifferentInputs_DifferentKeys(t *testing.T) {
	t.Parallel()
	k1 := BuildObjectKey("gen_a", "https://x/1", "image/png")
	k2 := BuildObjectKey("gen_a", "https://x/2", "image/png")
	if k1 == k2 {
		t.Errorf("different URLs should yield different keys, both got %q", k1)
	}
	k3 := BuildObjectKey("gen_b", "https://x/1", "image/png")
	if k1 == k3 {
		t.Errorf("different task IDs should yield different keys, both got %q", k1)
	}
}

func TestBuildObjectKey_SanitizesTaskID(t *testing.T) {
	t.Parallel()
	// Defensive: even garbage task IDs must produce a safe path.
	k := BuildObjectKey("../etc/passwd", "https://x", "image/png")
	if strings.Contains(k, "..") {
		t.Errorf("sanitized key still contains '..': %q", k)
	}
	if strings.Contains(k, "/etc/") {
		t.Errorf("sanitized key still contains '/etc/': %q", k)
	}
}

func TestExtensionFor_KnownAndUnknown(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"image/png":          ".png",
		"IMAGE/PNG":          ".png",
		"image/jpeg":         ".jpg",
		"audio/wav":          ".wav",
		"video/mp4":          ".mp4",
		"":                   ".bin",
		"  ":                 ".bin",
		"unknown/x-format":   ".bin",
		"text/plain":         ".txt",
		"image/png; param=1": ".png",
	}
	for mime, want := range cases {
		if got := extensionFor(mime); got != want {
			t.Errorf("extensionFor(%q): want %q, got %q", mime, want, got)
		}
	}
}
