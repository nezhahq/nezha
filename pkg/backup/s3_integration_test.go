package backup

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS3Integration runs the whole backup cycle against real S3 compatible
// storage (MinIO, Cloudflare R2, ...). It is skipped unless
// NZ_TEST_S3_ENDPOINT, NZ_TEST_S3_BUCKET, NZ_TEST_S3_ACCESS_KEY_ID and
// NZ_TEST_S3_SECRET_ACCESS_KEY are set (optional: NZ_TEST_S3_REGION,
// NZ_TEST_S3_PATH_STYLE=false). Every run uses its own prefix and deletes
// what it wrote.
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("NZ_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("NZ_TEST_S3_ENDPOINT not set")
	}
	region := os.Getenv("NZ_TEST_S3_REGION")
	if region == "" {
		region = DefaultRegion
	}
	prefix, err := randomHex(6)
	require.NoError(t, err)
	prefix = "nezha-backup-it/" + prefix + "/"

	e := newTestEnv(t)
	pathStyle := os.Getenv("NZ_TEST_S3_PATH_STYLE") != "false"
	// The test MinIO listens on plain http on loopback; production never
	// allows that.
	e.m.opts.AllowLoopbackHTTP = true
	e.m.opts.NewStorage = func(s S3Settings) (Storage, error) { return newS3Storage(s, true) }
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{
		Endpoint: str(endpoint), Bucket: str(os.Getenv("NZ_TEST_S3_BUCKET")), Region: str(region), Prefix: str(prefix),
		UsePathStyle: &pathStyle, AccessKeyID: str(os.Getenv("NZ_TEST_S3_ACCESS_KEY_ID")), SecretAccessKey: str(os.Getenv("NZ_TEST_S3_SECRET_ACCESS_KEY")),
	}}, admin)
	require.NoError(t, err)
	_, _, err = e.m.SetPassword(testPassword, okReauth, admin)
	require.NoError(t, err)
	ctx := context.Background()
	t.Cleanup(func() {
		// The manager refuses work after Apply; clean up through the storage.
		s, _ := e.m.effective()
		st, err := newS3Storage(s.S3, true)
		if err != nil {
			return
		}
		objs, _ := st.List(ctx, prefix)
		for _, o := range objs {
			_ = st.Delete(ctx, o.Key)
		}
		left, _ := st.List(ctx, prefix)
		assert.Empty(t, left, "integration test objects must be removed")
	})

	require.NoError(t, e.m.TestConnection(ctx, nil, admin))

	keep := 1
	_, err = e.m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Retention: &keep}}, admin)
	require.NoError(t, err)
	manual := e.backup(TriggerManual)
	require.Equal(t, StatusSuccess, manual.Status, manual.Error)
	e.backup(TriggerSchedule)
	latest := e.backup(TriggerSchedule)
	list, err := e.m.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2, "retention keeps one scheduled backup next to the manual one")
	for _, b := range list {
		assert.NotEmpty(t, b.SHA256, "metadata is read back from the storage")
	}

	rc, info, err := e.m.Open(ctx, latest.Name, admin)
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, rc)
	rc.Close()
	require.NoError(t, err)
	assert.Equal(t, info.Size, n)

	res, err := e.prepare(manual, testPassword)
	require.NoError(t, err)
	require.True(t, res.CanApply, "%+v", res.Checks)
	require.NoError(t, e.apply(res, testPassword))
}
