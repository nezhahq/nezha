package rpc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	rpcService "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

var unaryMethods = []string{"ReportSystemInfo", "ReportSystemInfo2", "ReportGeoIP"}

func serveH2C(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// callWithheldMessage opens a unary call and never sends its request message.
// It returns the grpc-status the call was ended with.
func callWithheldMessage(t *testing.T, server *httptest.Server, method string, headers ...string) string {
	t.Helper()
	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	withheldMessage, neverWritten := io.Pipe()
	t.Cleanup(func() { neverWritten.Close() })

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		server.URL+"/"+proto.NezhaService_ServiceDesc.ServiceName+"/"+method, withheldMessage)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/grpc")
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("call was not answered while its message was withheld: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("call was not ended while its message was withheld: %v", err)
	}
	if grpcStatus := response.Header.Get("Grpc-Status"); grpcStatus != "" {
		return grpcStatus
	}
	return response.Trailer.Get("Grpc-Status")
}

// Unary interceptors and handlers run only once the request message has
// arrived, so without admitUnaryAtHeaders a peer with no agent secret that
// withholds the message keeps the call, its goroutines and its read buffer
// parked in the dashboard for good.
func TestUnaryCallWithoutSecretIsRefusedBeforeItsMessageArrives(t *testing.T) {
	withRealIPHeader(t, "")
	originalHandler, originalHook := rpcService.NezhaHandlerSingleton, singleton.ServerTransferStreamRevocationHook
	t.Cleanup(func() {
		rpcService.NezhaHandlerSingleton, singleton.ServerTransferStreamRevocationHook = originalHandler, originalHook
	})
	server := serveH2C(t, ServeRPC())

	for _, method := range unaryMethods {
		t.Run(method, func(t *testing.T) {
			if got := callWithheldMessage(t, server, method); got != "16" {
				t.Fatalf("grpc-status = %q, want 16 (Unauthenticated)", got)
			}
		})
	}
}

// The same holds for a peer the real-IP gate refuses, whatever it presents.
func TestUnaryCallWithoutRealIPIsRefusedBeforeItsMessageArrives(t *testing.T) {
	withRealIPHeader(t, "X-Real-IP")
	authenticated := func(context.Context) (uint64, error) {
		t.Error("auth check ran for a call the real-IP gate refused")
		return 0, nil
	}
	server := serveH2C(t, admitUnaryAtHeaders(grpc.NewServer(), authenticated, time.Minute))

	if got := callWithheldMessage(t, server, "ReportSystemInfo2"); got != "2" {
		t.Fatalf("grpc-status = %q, want 2 (Unknown)", got)
	}
}

// Nor may an address on the WAF block list park calls the dashboard would
// refuse as soon as their message arrived.
func TestUnaryCallFromBlockedIPIsRefusedBeforeItsMessageArrives(t *testing.T) {
	withRealIPHeader(t, "X-Real-IP")
	originalDB := singleton.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(model.WAF{}))
	singleton.DB = db
	t.Cleanup(func() {
		singleton.DB = originalDB
		_ = sqlDB.Close()
	})
	require.NoError(t, model.BlockIP(db, "203.0.113.9", model.WAFBlockReasonTypeManual, model.BlockIDManual))

	authenticated := func(context.Context) (uint64, error) {
		t.Error("auth check ran for a call the WAF refused")
		return 0, nil
	}
	server := serveH2C(t, admitUnaryAtHeaders(grpc.NewServer(), authenticated, time.Minute))

	for _, method := range unaryMethods {
		t.Run(method, func(t *testing.T) {
			if got := callWithheldMessage(t, server, method, "X-Real-IP", "203.0.113.9"); got != "2" {
				t.Fatalf("grpc-status = %q, want 2 (Unknown)", got)
			}
		})
	}
}

// A peer that holds a secret must not be able to park the call either.
func TestAdmittedUnaryCallMustDeliverItsMessageInTime(t *testing.T) {
	withRealIPHeader(t, "")
	grpcServer := grpc.NewServer()
	proto.RegisterNezhaServiceServer(grpcServer, proto.UnimplementedNezhaServiceServer{})
	authenticated := func(context.Context) (uint64, error) { return 1, nil }
	server := serveH2C(t, admitUnaryAtHeaders(grpcServer, authenticated, 100*time.Millisecond))

	for _, method := range unaryMethods {
		t.Run(method, func(t *testing.T) {
			if got := callWithheldMessage(t, server, method); got == "" || got == "0" {
				t.Fatalf("grpc-status = %q, want an error status", got)
			}
		})
	}
}

// Streaming calls are decided by the stream interceptors and their handlers,
// and live longer than any message deadline.
func TestStreamingCallsAreNotAdmittedAtHeaders(t *testing.T) {
	withRealIPHeader(t, "")
	authenticated := func(context.Context) (uint64, error) {
		t.Error("auth check ran at the headers of a streaming call")
		return 0, nil
	}
	for _, stream := range proto.NezhaService_ServiceDesc.Streams {
		reached := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		request := httptest.NewRequest(http.MethodPost,
			"/"+proto.NezhaService_ServiceDesc.ServiceName+"/"+stream.StreamName, strings.NewReader(""))
		admitUnaryAtHeaders(next, authenticated, time.Minute).ServeHTTP(httptest.NewRecorder(), request)
		if !reached {
			t.Fatalf("%s did not reach the gRPC server", stream.StreamName)
		}
	}
}

// An agent sees the same status for a refused call as it did when the handler
// refused it, and an admitted call is served as before.
func TestUnaryAdmissionIsTransparentToAgents(t *testing.T) {
	withRealIPHeader(t, "")
	grpcServer := grpc.NewServer()
	proto.RegisterNezhaServiceServer(grpcServer, proto.UnimplementedNezhaServiceServer{})
	secret := func(ctx context.Context) (uint64, error) {
		if len(metadata.ValueFromIncomingContext(ctx, "client_secret")) == 0 {
			return 0, status.Error(codes.Unauthenticated, "客户端认证失败 100%")
		}
		return 1, nil
	}
	server := serveH2C(t, admitUnaryAtHeaders(grpcServer, secret, time.Minute))

	connection, err := grpc.NewClient(strings.TrimPrefix(server.URL, "http://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := proto.NewNezhaServiceClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.ReportSystemInfo2(ctx, &proto.Host{})
	if got := status.Convert(err); got.Code() != codes.Unauthenticated || got.Message() != "客户端认证失败 100%" {
		t.Fatalf("call without secret = %v, want Unauthenticated with the auth check's message", err)
	}

	_, err = client.ReportSystemInfo2(ctx, &proto.Host{}, grpc.PerRPCCredentials(agentSecret("secret")))
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("call with secret = %v, want it to reach the handler (Unimplemented)", err)
	}
}

type agentSecret string

func (secret agentSecret) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"client_secret": string(secret)}, nil
}

func (agentSecret) RequireTransportSecurity() bool { return false }
