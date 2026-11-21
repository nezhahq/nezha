package s3store

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mock: fake credentials for the fake S3 server below.
const (
	mockAccessKeyID     = "fake-access-key-for-tests"
	mockSecretAccessKey = "fake-secret-key-for-tests"
)

type fakeObject struct {
	data []byte
	meta http.Header
}

// fakeS3 implements the handful of S3 calls the store makes (path-style).
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]fakeObject
	auth    []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Has("location"):
		_, _ = io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
	case r.Method == http.MethodGet && key == "":
		f.list(w, r)
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		meta := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				meta[k] = v
			}
		}
		f.objects[key] = fakeObject{data: data, meta: meta}
		w.Header().Set("ETag", `"etag-`+key+`"`)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		o, ok := f.objects[key]
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		for k, v := range o.meta {
			w.Header()[k] = v
		}
		w.Header().Set("ETag", `"etag-`+key+`"`)
		w.Header().Set("Last-Modified", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(o.data)
		}
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type content struct {
		Key          string
		Size         int
		ETag         string
		LastModified string
	}
	type result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Name        string
		Prefix      string
		KeyCount    int
		MaxKeys     int
		IsTruncated bool
		Contents    []content
	}
	res := result{Name: f.bucket, Prefix: prefix, MaxKeys: 1000}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[k].data), ETag: `"etag-` + k + `"`, LastModified: "2026-01-01T00:00:00.000Z"})
	}
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<Error><Code>`+code+`</Code><Message>`+code+` from fake</Message></Error>`)
}

func newFakeStore(t *testing.T, bucket string) (*Store, *fakeS3) {
	t.Helper()
	fake := &fakeS3{bucket: "nezha-test", objects: map[string]fakeObject{}}
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)
	st, err := New(Config{Endpoint: srv.URL, Bucket: bucket, Region: "us-east-1", UsePathStyle: true,
		AccessKeyID: mockAccessKeyID, SecretAccessKey: mockSecretAccessKey, Transport: srv.Client().Transport})
	require.NoError(t, err)
	return st, fake
}

func TestStoreRoundTrip(t *testing.T) {
	st, fake := newFakeStore(t, "nezha-test")
	ctx := context.Background()
	body := "encrypted backup bytes"
	_, err := st.Put(ctx, "nezha-backup/a.nzbak", strings.NewReader(body), int64(len(body)), map[string]string{"sha256": "abc", "nezha-version": "v1.2.3"})
	require.NoError(t, err)

	info, err := st.Stat(ctx, "nezha-backup/a.nzbak")
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), info.Size)
	assert.Equal(t, "abc", info.Metadata["sha256"], "user metadata is returned with lower-case keys")
	assert.Equal(t, "v1.2.3", info.Metadata["nezha-version"])

	rc, _, err := st.Get(ctx, "nezha-backup/a.nzbak")
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	rc.Close()
	assert.Equal(t, body, string(got))

	list, err := st.List(ctx, "nezha-backup/")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "nezha-backup/a.nzbak", list[0].Key)

	require.NoError(t, st.Delete(ctx, "nezha-backup/a.nzbak"))
	_, err = st.Stat(ctx, "nezha-backup/a.nzbak")
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = st.Get(ctx, "nezha-backup/a.nzbak")
	require.ErrorIs(t, err, ErrNotFound)

	for _, a := range fake.auth {
		assert.NotContains(t, a, mockSecretAccessKey, "requests are signed, the secret is never sent")
	}
}

func TestStoreErrorsHideCredentials(t *testing.T) {
	st, _ := newFakeStore(t, "missing-bucket")
	_, err := st.Put(context.Background(), "k", strings.NewReader("x"), 1, nil)
	require.Error(t, err)
	var se *Error
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "NoSuchBucket", se.Code)
	assert.NotContains(t, err.Error(), mockSecretAccessKey)
	assert.NotContains(t, err.Error(), mockAccessKeyID)
}

func TestParseEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://abc.r2.cloudflarestorage.com": "abc.r2.cloudflarestorage.com",
		"https://s3.example.com:8443/":         "s3.example.com:8443",
		"https://127.0.0.1:9000":               "127.0.0.1:9000",
	} {
		host, _, err := ParseEndpoint(in, false)
		require.NoError(t, err, in)
		assert.Equal(t, want, host)
	}
	for _, bad := range []string{"", "s3.example.com", "http://s3.example.com", "http://127.0.0.1:9000", "http://localhost:9000",
		"https://u:p@s3.example.com", "https://s3.example.com/bucket", "https://s3.example.com?x=1", "ftp://s3.example.com"} {
		_, _, err := ParseEndpoint(bad, false)
		assert.Error(t, err, bad)
	}
	// Test-only: plain http to a loopback MinIO.
	for _, in := range []string{"http://127.0.0.1:9000", "http://localhost:9000", "http://[::1]:9000"} {
		_, secure, err := ParseEndpoint(in, true)
		require.NoError(t, err, in)
		assert.False(t, secure)
	}
	_, _, err := ParseEndpoint("http://10.0.0.1:9000", true)
	assert.Error(t, err, "only loopback addresses may use http even in tests")
}
