package rpc

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

func ctxWithRealIP(ctx context.Context) (context.Context, error) {
	var ip, connectingIp string
	p, ok := peer.FromContext(ctx)
	if ok {
		addrPort, err := netip.ParseAddrPort(p.Addr.String())
		if err == nil {
			connectingIp = addrPort.Addr().String()
		}
	}
	ctx = context.WithValue(ctx, model.CtxKeyConnectingIP{}, connectingIp)

	if singleton.Conf.AgentRealIPHeader == "" {
		return ctx, nil
	}

	if singleton.Conf.AgentRealIPHeader == model.ConfigUsePeerIP {
		if connectingIp == "" {
			return ctx, fmt.Errorf("connecting ip not found")
		}
		ip = connectingIp
	} else {
		vals := metadata.ValueFromIncomingContext(ctx, singleton.Conf.AgentRealIPHeader)
		if len(vals) == 0 {
			return ctx, fmt.Errorf("real ip header not found")
		}
		var err error
		ip, err = utils.GetIPFromHeader(vals[0])
		if err != nil {
			return ctx, err
		}
	}

	if singleton.Conf.Debug {
		log.Printf("NEZHA>> gRPC Agent Real IP: %s, connecting IP: %s\n", ip, connectingIp)
	}

	return context.WithValue(ctx, model.CtxKeyRealIP{}, ip), nil
}

func waf(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	realip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func getRealIp(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := ctxWithRealIP(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

type realIPServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *realIPServerStream) Context() context.Context { return s.ctx }

func getRealIpStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := ctxWithRealIP(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &realIPServerStream{ServerStream: ss, ctx: ctx})
}

func wafStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	realip, _ := ss.Context().Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return err
	}
	return handler(srv, ss)
}

// unaryMessageTimeout is how long an admitted unary call may take to deliver
// its request message. An agent sends it right behind the headers.
const unaryMessageTimeout = 30 * time.Second

// admitUnaryAtHeaders applies the real-IP, WAF and agent-secret checks to a
// unary call when its HTTP/2 headers arrive, which is when the stream
// interceptors and stream handlers apply them to a streaming call.
//
// grpc-go runs unary interceptors and handlers only once it has received and
// decoded the whole request message, and grpc.Server.ServeHTTP does not limit
// how long a peer may take to send it. getRealIp, waf and the handlers' auth
// check alone therefore never refuse a call whose message is withheld: it
// stays parked, with its goroutines and read buffer, for as long as the peer
// keeps the connection, whether or not the peer has a secret or is blocked.
// Everything those checks look at is in the headers, so the call is decided
// here; the interceptors still run afterwards and give the handler its real IP.
func admitUnaryAtHeaders(next http.Handler, auth func(context.Context) (uint64, error), messageTimeout time.Duration) http.Handler {
	unary := make(map[string]struct{}, len(proto.NezhaService_ServiceDesc.Methods))
	for _, method := range proto.NezhaService_ServiceDesc.Methods {
		unary["/"+proto.NezhaService_ServiceDesc.ServiceName+"/"+method.MethodName] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := unary[r.URL.Path]; !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx, err := ctxWithRealIP(headerContext(r))
		if err == nil {
			realip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
			err = model.CheckIP(singleton.DB, realip)
		}
		if err == nil {
			_, err = auth(ctx)
		}
		if err != nil {
			refuseCall(w, err)
			return
		}
		// Only reads of the request body are bound; a message that is
		// complete by then is not affected.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(messageTimeout))
		next.ServeHTTP(w, r)
	})
}

// headerContext is the context grpc.Server.ServeHTTP will derive for r, as far
// as ctxWithRealIP and the auth check read it: the peer address and the
// request headers as incoming metadata.
func headerContext(r *http.Request) context.Context {
	md := make(metadata.MD, len(r.Header))
	for name, values := range r.Header {
		md[strings.ToLower(name)] = values
	}
	ctx := metadata.NewIncomingContext(r.Context(), md)
	if addrPort, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: net.TCPAddrFromAddrPort(addrPort)})
	}
	return ctx
}

// refuseCall ends a call that is not handed to grpc.Server with the status of
// err, as a gRPC Trailers-Only response.
func refuseCall(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	var message strings.Builder
	for _, b := range []byte(st.Message()) {
		// grpc-message is percent-encoded outside printable ASCII.
		if b < ' ' || b > '~' || b == '%' {
			fmt.Fprintf(&message, "%%%02X", b)
		} else {
			message.WriteByte(b)
		}
	}
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", strconv.Itoa(int(st.Code())))
	w.Header().Set("Grpc-Message", message.String())
	w.WriteHeader(http.StatusOK)
}
