// Package s3store stores backups in S3 compatible object storage
// (AWS S3, Cloudflare R2, MinIO, ...) through minio-go.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// Endpoint and configuration errors.
var (
	ErrEndpointNotURL = errors.New("endpoint must be an absolute URL such as https://s3.example.com")
	ErrEndpointParts  = errors.New("endpoint must not contain credentials, a path, a query or a fragment")
	ErrEndpointScheme = errors.New("endpoint must use https://")
	ErrIncomplete     = errors.New("bucket and credentials are required")
)

// ObjectInfo describes a stored object. Metadata keys are lower-case.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	Metadata     map[string]string
}

// Config is the connection configuration of a bucket.
type Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	UsePathStyle    bool
	AccessKeyID     string
	SecretAccessKey string
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
	// AllowLoopbackHTTP accepts http:// to a loopback address (tests only).
	AllowLoopbackHTTP bool
}

// Store reads and writes objects of one bucket.
type Store struct {
	client *minio.Client
	bucket string
}

// ParseEndpoint validates an endpoint URL and returns its host[:port] and
// whether TLS is used. HTTPS is required. allowLoopbackHTTP additionally
// accepts plain HTTP to a loopback address; it exists for integration tests
// against a local MinIO and is never set from configuration.
func ParseEndpoint(endpoint string, allowLoopbackHTTP bool) (host string, secure bool, err error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", false, ErrEndpointNotURL
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false, ErrEndpointParts
	}
	switch {
	case u.Scheme == "https":
		return u.Host, true, nil
	case u.Scheme == "http" && allowLoopbackHTTP && isLoopback(u.Hostname()):
		return u.Host, false, nil
	default:
		return "", false, ErrEndpointScheme
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New returns a Store for cfg. It does not contact the server.
func New(cfg Config) (*Store, error) {
	host, secure, err := ParseEndpoint(cfg.Endpoint, cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, ErrIncomplete
	}
	lookup := minio.BucketLookupDNS
	if cfg.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	var transport http.RoundTripper = cfg.Transport
	if transport == nil {
		t, err := minio.DefaultTransport(secure)
		if err != nil {
			return nil, err
		}
		transport = t
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, err
	}
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, metadata map[string]string) (ObjectInfo, error) {
	info, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: metadata,
	})
	if err != nil {
		return ObjectInfo{}, wrapErr(err)
	}
	return ObjectInfo{Key: key, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified, Metadata: lowerKeys(metadata)}, nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, wrapErr(err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectInfo{}, wrapErr(err)
	}
	return obj, convert(st), nil
}

func (s *Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, wrapErr(err)
	}
	return convert(st), nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if obj.Err != nil {
			return nil, wrapErr(obj.Err)
		}
		out = append(out, convert(obj))
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return wrapErr(s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}))
}

func convert(o minio.ObjectInfo) ObjectInfo {
	meta := make(map[string]string, len(o.UserMetadata))
	for k, v := range o.UserMetadata {
		k = strings.ToLower(k)
		k = strings.TrimPrefix(k, "x-amz-meta-")
		meta[k] = v
	}
	return ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified, ETag: o.ETag, Metadata: meta}
}

func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// Error is a storage error safe to show to an administrator: it carries the
// S3 error code and message, never request signatures or credentials.
type Error struct {
	Code       string
	Message    string
	StatusCode int
}

func (e *Error) Error() string {
	if e.Code == "" {
		return "storage error: " + e.Message
	}
	return fmt.Sprintf("storage error %s: %s", e.Code, e.Message)
}

func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound && resp.Code != "NoSuchBucket" {
		return ErrNotFound
	}
	if resp.Code != "" {
		return &Error{Code: resp.Code, Message: resp.Message, StatusCode: resp.StatusCode}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// The URL holds only the endpoint and object key, never credentials.
		return &Error{Message: urlErr.Err.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Message: "request timed out"}
	}
	return &Error{Message: err.Error()}
}

// DefaultTimeout bounds a single storage call made on behalf of a request.
const DefaultTimeout = 30 * time.Second
