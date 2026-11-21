package backup

import (
	"context"
	"errors"
	"io"

	"github.com/nezhahq/nezha/pkg/backup/s3store"
)

// s3Storage adapts s3store.Store to Storage.
type s3Storage struct{ s *s3store.Store }

func newS3Storage(s S3Settings, allowLoopbackHTTP bool) (Storage, error) {
	cfg := s.storeConfig()
	cfg.AllowLoopbackHTTP = allowLoopbackHTTP
	st, err := s3store.New(cfg)
	if err != nil {
		return nil, err
	}
	return s3Storage{st}, nil
}

func fromS3(o s3store.ObjectInfo) ObjectInfo {
	return ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified, ETag: o.ETag, Metadata: o.Metadata}
}

func s3Err(err error) error {
	if errors.Is(err, s3store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (a s3Storage) Put(ctx context.Context, key string, r io.Reader, size int64, metadata map[string]string) (ObjectInfo, error) {
	o, err := a.s.Put(ctx, key, r, size, metadata)
	return fromS3(o), s3Err(err)
}

func (a s3Storage) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	rc, o, err := a.s.Get(ctx, key)
	return rc, fromS3(o), s3Err(err)
}

func (a s3Storage) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	o, err := a.s.Stat(ctx, key)
	return fromS3(o), s3Err(err)
}

func (a s3Storage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	objs, err := a.s.List(ctx, prefix)
	if err != nil {
		return nil, s3Err(err)
	}
	out := make([]ObjectInfo, len(objs))
	for i, o := range objs {
		out[i] = fromS3(o)
	}
	return out, nil
}

func (a s3Storage) Delete(ctx context.Context, key string) error {
	return s3Err(a.s.Delete(ctx, key))
}
