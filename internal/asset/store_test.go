package asset

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewLocalDiskStore_RequiresArgs(t *testing.T) {
	t.Parallel()

	_, err := NewLocalDiskStore("", "https://cdn.modelhub.local/")
	if !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("empty rootDir: want ErrStorageNotConfigured, got %v", err)
	}
	_, err = NewLocalDiskStore(t.TempDir(), "")
	if !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("empty prefix: want ErrStorageNotConfigured, got %v", err)
	}
	_, err = NewLocalDiskStore(t.TempDir(), "https://cdn.modelhub.local")
	if !errors.Is(err, ErrStorageNotConfigured) {
		t.Errorf("prefix without trailing slash: want ErrStorageNotConfigured, got %v", err)
	}
}

func TestLocalDiskStore_Put_Roundtrip(t *testing.T) {
	t.Parallel()
	st := mustLocalStore(t)

	body := strings.NewReader("hello world")
	res, err := st.Put(context.Background(), "outputs/task-1/abc.bin", "application/octet-stream", body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.SizeBytes != int64(len("hello world")) {
		t.Errorf("size: want %d, got %d", len("hello world"), res.SizeBytes)
	}
	if !strings.HasPrefix(res.URL, "https://cdn.modelhub.local/") {
		t.Errorf("URL: want CDN prefix, got %q", res.URL)
	}
	if res.Key != "outputs/task-1/abc.bin" {
		t.Errorf("key: want %q, got %q", "outputs/task-1/abc.bin", res.Key)
	}

	// Confirm bytes hit disk.
	got, err := os.ReadFile(filepath.Join(st.RootDir, "outputs", "task-1", "abc.bin"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("disk contents: want %q, got %q", "hello world", string(got))
	}
}

func TestLocalDiskStore_Put_RejectsTraversal(t *testing.T) {
	t.Parallel()
	st := mustLocalStore(t)

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"absolute-unix", "/etc/passwd"},
		{"absolute-windows", "\\Windows\\evil"},
		{"dotdot", "outputs/../escape"},
		{"drive-prefix", "C:foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.Put(context.Background(), tc.key, "x", strings.NewReader("nope"))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestLocalDiskStore_Put_NilBody(t *testing.T) {
	t.Parallel()
	st := mustLocalStore(t)

	_, err := st.Put(context.Background(), "outputs/task-x/y.bin", "x", nil)
	if err == nil {
		t.Fatal("expected error for nil body, got nil")
	}
}

// TestLocalDiskStore_ConcurrentSameKey verifies that two concurrent
// uploads to the same key don't corrupt each other's data — one wins,
// the other writes its own bytes; in either case the file must contain
// a complete, valid payload (not a half-mixed one).
func TestLocalDiskStore_ConcurrentSameKey(t *testing.T) {
	t.Parallel()
	st := mustLocalStore(t)
	key := "outputs/task-collide/a.bin"

	const N = 10
	bodies := make([]string, N)
	for i := 0; i < N; i++ {
		bodies[i] = strings.Repeat(string(rune('a'+i)), 4096)
	}
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			if _, err := st.Put(context.Background(), key, "x", strings.NewReader(bodies[i])); err != nil {
				t.Errorf("put #%d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got, err := os.ReadFile(filepath.Join(st.RootDir, "outputs", "task-collide", "a.bin"))
	if err != nil {
		t.Fatalf("read after concurrent: %v", err)
	}
	// One of the bodies must win whole.
	matched := false
	for _, b := range bodies {
		if string(got) == b {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("file content does not match any of the N bodies (got %d bytes)", len(got))
	}
}

func TestLocalDiskStore_Put_RespectsCtxCancel(t *testing.T) {
	t.Parallel()
	st := mustLocalStore(t)

	// Build a slow reader that never returns.
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	_, err := st.Put(ctx, "outputs/task-cancel/a.bin", "x", pr)
	if err == nil {
		t.Fatal("expected ctx cancellation error, got nil")
	}
}

func mustLocalStore(t *testing.T) *LocalDiskStore {
	t.Helper()
	st, err := NewLocalDiskStore(t.TempDir(), "https://cdn.modelhub.local/")
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	return st
}
