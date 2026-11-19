package controller

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// holdDownloadSpoolShares stands in for downloads a user has parked (agent
// stalled, or HTTP client not reading the body): each keeps one share taken.
func holdDownloadSpoolShares(t *testing.T, userID uint64) int {
	t.Helper()
	held := 0
	for held <= cap(downloadSpoolSlots) {
		release, ok := acquireDownloadSpoolShare(userID)
		if !ok {
			break
		}
		t.Cleanup(release)
		held++
	}
	return held
}

// downloadSpoolSlots is one budget for every tenant. A single user must not be
// able to take all of it, otherwise 32 parked downloads on that user's own
// server refuse every other user's download for up to maxTransferDuration.
func TestDownloadSpoolShare_OneUserCannotTakeEverySlot(t *testing.T) {
	held := holdDownloadSpoolShares(t, 1)
	require.Equal(t, maxDownloadSpoolsPerUser, held)
	require.Less(t, held, cap(downloadSpoolSlots),
		"one user's share must leave process-wide spool slots for other users")

	release, ok := acquireDownloadSpoolShare(2)
	require.True(t, ok, "another user must still be admitted")
	release()
	release() // idempotent: must not hand user 2 an extra share
	require.Equal(t, maxDownloadSpoolsPerUser, holdDownloadSpoolShares(t, 2))
}

func TestTransferDownload_RejectsBeyondUserSpoolShare(t *testing.T) {
	ts, tok, cleanup := setupTransferTest(t, xferAgentDownloadSend([]byte("ok")))
	defer cleanup()
	srv, _ := singleton.ServerShared.Get(7)
	agent := srv.GetTaskStream()
	tasks := &countingTaskStream{xferStreamMux: agent.(*xferStreamMux)}
	srv.SetTaskStream(tasks)
	url := mintDownloadURL(t, ts, tok, "/srv/file")

	holdDownloadSpoolShares(t, 100)

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusTooManyRequests, resp.StatusCode, "body=%s", string(body))
	require.Equal(t, errTooManyDownloadsForUser.Error(), string(body))
	require.Zero(t, tasks.sent.Load(), "a download over the user's share must be refused before the agent is contacted")
	require.Equal(t, int64(1), countAuditRows(t, "fs.download", model.MCPOutcomeRateLimited))
}

func TestTransferDownload_OtherUsersSpoolShareDoesNotBlock(t *testing.T) {
	want := []byte("quarterly numbers\n")
	ts, tok, cleanup := setupTransferTest(t, xferAgentDownloadSend(want))
	defer cleanup()
	url := mintDownloadURL(t, ts, tok, "/srv/file")

	holdDownloadSpoolShares(t, 101)

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body=%s", string(body))
	require.Equal(t, want, body)

	require.Eventually(t, func() bool {
		downloadSpoolUsersMu.Lock()
		defer downloadSpoolUsersMu.Unlock()
		return downloadSpoolUsers[100] == 0
	}, time.Second, 10*time.Millisecond, "a finished download must give its share back")
}

type countingTaskStream struct {
	*xferStreamMux
	sent atomic.Int32
}

func (s *countingTaskStream) Send(t *pb.Task) error {
	if t.GetType() == model.TaskTypeFsTransfer {
		s.sent.Add(1)
	}
	return s.xferStreamMux.Send(t)
}
