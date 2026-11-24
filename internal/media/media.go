// Package media stores uploaded images and videos.
//
// The API depends on the Store interface, so local development uses the
// filesystem while production can use Google Cloud Storage without any
// handler changes.
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

type Store interface {
	// Put stores r under key and returns a URL clients can fetch it from.
	Put(ctx context.Context, key string, r io.Reader, contentType string) (string, error)
	Delete(ctx context.Context, key string) error
}

// Kind classifies an upload by sniffing its first bytes rather than trusting
// the file extension or the client-supplied Content-Type.
func Kind(head []byte, filename string) (kind, contentType string, ok bool) {
	ct := http.DetectContentType(head)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image", ct, true
	case strings.HasPrefix(ct, "video/"):
		return "video", ct, true
	}
	// DetectContentType does not recognise QuickTime; accept .mov by
	// extension only when the content sniffing was inconclusive.
	if ct == "application/octet-stream" && strings.EqualFold(filepath.Ext(filename), ".mov") {
		return "video", "video/quicktime", true
	}
	return "", ct, false
}

// Object is a stored media file, as seen by the orphan sweep.
type Object struct {
	Key     string
	ModTime time.Time
}

// Lister is implemented by stores that can enumerate their objects.
type Lister interface {
	List(ctx context.Context, fn func(Object) error) error
}

// ---------- local filesystem ----------

type LocalStore struct {
	Dir     string
	BaseURL string
}

func NewLocal(dir, baseURL string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{Dir: dir, BaseURL: strings.TrimRight(baseURL, "/")}, nil
}

func (s *LocalStore) path(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid media key %q", key)
	}
	return filepath.Join(s.Dir, key), nil
}

func (s *LocalStore) Put(_ context.Context, key string, r io.Reader, _ string) (string, error) {
	p, err := s.path(key)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Rename is atomic: readers never see a half-written file.
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return s.BaseURL + "/" + key, nil
}

func (s *LocalStore) List(_ context.Context, fn func(Object) error) error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // skip in-progress ".upload-*" temp files
		}
		info, err := e.Info()
		if err != nil {
			continue // removed concurrently
		}
		if err := fn(Object{Key: e.Name(), ModTime: info.ModTime()}); err != nil {
			return err
		}
	}
	return nil
}

func (s *LocalStore) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------- Google Cloud Storage ----------

// GCSStore writes objects to a bucket. Read access is expected to be granted
// at the bucket level (uniform bucket-level access); per-object public ACLs,
// as the original version used, are disabled on new buckets by default.
type GCSStore struct {
	client *storage.Client
	bucket string
}

func NewGCS(ctx context.Context, bucket string) (*GCSStore, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GCSStore{client: c, bucket: bucket}, nil
}

func (s *GCSStore) Put(ctx context.Context, key string, r io.Reader, contentType string) (string, error) {
	w := s.client.Bucket(s.bucket).Object(key).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", s.bucket, key), nil
}

func (s *GCSStore) List(ctx context.Context, fn func(Object) error) error {
	it := s.client.Bucket(s.bucket).Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(Object{Key: attrs.Name, ModTime: attrs.Updated}); err != nil {
			return err
		}
	}
}

func (s *GCSStore) Delete(ctx context.Context, key string) error {
	err := s.client.Bucket(s.bucket).Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}
