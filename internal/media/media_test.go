package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestKindSniffsContentNotExtension(t *testing.T) {
	cases := []struct {
		name     string
		head     []byte
		filename string
		kind     string
		ok       bool
	}{
		{"png", pngHeader, "a.png", "image", true},
		{"png renamed to .txt is still an image", pngHeader, "a.txt", "image", true},
		{"html disguised as .jpg is rejected", []byte("<html><script>alert(1)</script>"), "evil.jpg", "", false},
		{"mp4", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), "v.mp4", "video", true},
		{"unknown binary .mov accepted as video", []byte{0, 0, 0, 0x14, 'w', 'i', 'd', 'e', 1, 2, 3}, "clip.MOV", "video", true},
		{"unknown binary .bin rejected", []byte{0, 1, 2, 3, 4}, "x.bin", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, _, ok := Kind(c.head, c.filename)
			if ok != c.ok || kind != c.kind {
				t.Fatalf("Kind() = %q, %v; want %q, %v", kind, ok, c.kind, c.ok)
			}
		})
	}
}

func TestLocalStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocal(dir, "http://x/media/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	url, err := s.Put(ctx, "abc.png", strings.NewReader("data"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://x/media/abc.png" {
		t.Errorf("url = %q", url)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "abc.png")); string(b) != "data" {
		t.Errorf("file content = %q", b)
	}

	for _, bad := range []string{"../escape", "a/b", `a\b`, ""} {
		if _, err := s.Put(ctx, bad, strings.NewReader("x"), ""); err == nil {
			t.Errorf("key %q should be rejected (path traversal)", bad)
		}
	}

	if err := s.Delete(ctx, "abc.png"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "abc.png"); err != nil {
		t.Errorf("deleting a missing file should be a no-op, got %v", err)
	}
}
