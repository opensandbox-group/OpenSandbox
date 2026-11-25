package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const grpcTestSandbox = "test-sandbox"

type grpcBackend struct {
	port   string
	health *health.Server
	mu     sync.Mutex
	seen   []metadata.MD
}

func (b *grpcBackend) lastMetadata() metadata.MD {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) == 0 {
		return nil
	}
	return b.seen[len(b.seen)-1]
}

// startGRPCBackend runs a real gRPC server (health service) that records the
// metadata of every call it receives.
func startGRPCBackend(t *testing.T) *grpcBackend {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &grpcBackend{health: health.NewServer()}
	_, backend.port, _ = net.SplitHostPort(listener.Addr().String())
	record := func(ctx context.Context) {
		md, _ := metadata.FromIncomingContext(ctx)
		backend.mu.Lock()
		backend.seen = append(backend.seen, md)
		backend.mu.Unlock()
	}
	server := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			record(ctx)
			return handler(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			record(ss.Context())
			return handler(srv, ss)
		}),
	)
	healthpb.RegisterHealthServer(server, backend.health)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return backend
}

// startH2CIngress serves the proxy the way main.go does: HTTP/1.1 and h2c on
// one port.
func startH2CIngress(t *testing.T) string {
	t.Helper()
	Logger = slogger.MustNew(slogger.Config{Level: "debug"})
	provider := &mockProvider{endpoints: map[string]string{grpcTestSandbox: "127.0.0.1"}}
	server := httptest.NewUnstartedServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil))
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func dialIngress(t *testing.T, addr string, opts ...grpc.DialOption) healthpb.HealthClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return healthpb.NewHealthClient(conn)
}

func routedContext(t *testing.T, route string, extra ...string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, append([]string{"opensandbox-ingress-to", route}, extra...)...)
}

func h2cClient() *http.Client {
	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: transport}
}

func TestGRPCUnaryThroughIngress(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t))

	resp, err := client.Check(routedContext(t, grpcTestSandbox+"-"+backend.port), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check through ingress: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status = %v, want SERVING", resp.GetStatus())
	}
}

func TestGRPCServerStreamingThroughIngress(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t))

	stream, err := client.Watch(routedContext(t, grpcTestSandbox+"-"+backend.port), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("first Recv = %v, %v; want SERVING", first.GetStatus(), err)
	}
	backend.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	second, err := stream.Recv()
	if err != nil || second.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("second Recv = %v, %v; want NOT_SERVING", second.GetStatus(), err)
	}
}

func TestGRPCErrorTrailersPreserved(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t))

	_, err := client.Check(routedContext(t, grpcTestSandbox+"-"+backend.port), &healthpb.HealthCheckRequest{Service: "unknown"})
	if status.Code(err) != codes.NotFound || !strings.Contains(status.Convert(err).Message(), "unknown service") {
		t.Fatalf("err = %v; want NotFound from the backend", err)
	}
}

func TestGRPCRoutingMetadataIsStripped(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t))

	ctx := routedContext(t, grpcTestSandbox+"-"+backend.port,
		"opensandbox-secure-access", "token",
		"opensandbox-access-renew", "skip")
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	md := backend.lastMetadata()
	for _, key := range []string{"opensandbox-ingress-to", "opensandbox-secure-access", "opensandbox-access-renew"} {
		if values := md.Get(key); len(values) > 0 {
			t.Errorf("backend received %s=%v", key, values)
		}
	}
}

func TestGRPCRoutingByAuthority(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t), grpc.WithAuthority(grpcTestSandbox+"-"+backend.port+".example.com"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("Check routed by :authority: %v", err)
	}
}

func TestGRPCFullDuplexIsNotBuffered(t *testing.T) {
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	}))
	backend.Config.Protocols = new(http.Protocols)
	backend.Config.Protocols.SetUnencryptedHTTP2(true)
	backend.Start()
	t.Cleanup(backend.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, writer := io.Pipe()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+startH2CIngress(t)+"/echo", body)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set(SandboxIngress, grpcTestSandbox+"-"+port)
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	for i := 1; i <= 2; i++ {
		msg := fmt.Sprintf("ping%d", i)
		if _, err := io.WriteString(writer, msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != msg {
			t.Fatalf("echo %d = %q, %v; want %q before the request ends", i, got, err, msg)
		}
	}
	_ = writer.Close()
}

// The two tests below guard the non-gRPC paths: they pass today and must
// keep passing once gRPC takes its own upstream transport.

func startProtoEchoBackend(t *testing.T) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	t.Cleanup(backend.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	return port
}

func h2cRequestBody(t *testing.T, method, contentType, port string) string {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+startH2CIngress(t)+"/", strings.NewReader(""))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set(SandboxIngress, grpcTestSandbox+"-"+port)
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	return string(body)
}

func TestGRPCWebOverH2UsesHTTP1Upstream(t *testing.T) {
	if got := h2cRequestBody(t, http.MethodPost, "application/grpc-web+proto", startProtoEchoBackend(t)); got != "HTTP/1.1" {
		t.Fatalf("upstream proto = %q, want HTTP/1.1", got)
	}
}

func TestPlainH2CRequestReachesHTTP1Backend(t *testing.T) {
	if got := h2cRequestBody(t, http.MethodGet, "", startProtoEchoBackend(t)); got != "HTTP/1.1" {
		t.Fatalf("upstream proto = %q, want HTTP/1.1", got)
	}
}

func TestIsGRPCRequest(t *testing.T) {
	for _, test := range []struct {
		protoMajor  int
		contentType string
		want        bool
	}{
		{2, "application/grpc", true},
		{2, "application/grpc+proto", true},
		{2, "Application/GRPC; charset=utf-8", true},
		{2, "application/grpc-web", false},
		{2, "application/grpc-web+proto", false},
		{2, "application/grpc-web-text", false},
		{1, "application/grpc", false},
		{2, "application/json", false},
		{2, "", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.ProtoMajor = test.protoMajor
		r.Header.Set("Content-Type", test.contentType)
		if got := isGRPCRequest(r); got != test.want {
			t.Errorf("isGRPCRequest(HTTP/%d, %q) = %v, want %v", test.protoMajor, test.contentType, got, test.want)
		}
	}
}

func TestGRPCCodeForHTTPStatus(t *testing.T) {
	for status, want := range map[int]codes.Code{
		http.StatusBadRequest:          codes.InvalidArgument,
		http.StatusUnauthorized:        codes.Unauthenticated,
		http.StatusForbidden:           codes.PermissionDenied,
		http.StatusNotFound:            codes.NotFound,
		http.StatusNotImplemented:      codes.Unimplemented,
		http.StatusBadGateway:          codes.Unavailable,
		http.StatusServiceUnavailable:  codes.Unavailable,
		http.StatusGatewayTimeout:      codes.Unavailable,
		http.StatusInternalServerError: codes.Unknown,
		http.StatusTeapot:              codes.Unknown,
	} {
		if got := grpcCodeForHTTPStatus(status); got != want {
			t.Errorf("grpcCodeForHTTPStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestWriteIngressErrorPlainHTTPUnchanged(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeIngressError(recorder, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusNotFound, "OpenSandbox Ingress: gone")
	if recorder.Code != http.StatusNotFound || recorder.Body.String() != "OpenSandbox Ingress: gone\n" {
		t.Fatalf("got %d %q; want what http.Error writes", recorder.Code, recorder.Body.String())
	}
}

func TestWriteIngressErrorGRPCTrailersOnly(t *testing.T) {
	recorder := httptest.NewRecorder()
	sw := &statusCapturingResponseWriter{ResponseWriter: recorder, statusCode: http.StatusOK}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	sw.Header().Set("Retry-After", "1")

	writeIngressError(sw, r, http.StatusNotFound, "OpenSandbox Ingress: sandbox not found: x")

	message, _ := url.PathUnescape(recorder.Header().Get("Grpc-Message"))
	switch {
	case recorder.Code != http.StatusOK:
		t.Errorf("HTTP status = %d, want 200 (trailers-only)", recorder.Code)
	case recorder.Header().Get("Content-Type") != "application/grpc":
		t.Errorf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	case recorder.Header().Get("Grpc-Status") != "5":
		t.Errorf("Grpc-Status = %q, want 5", recorder.Header().Get("Grpc-Status"))
	case message != "OpenSandbox Ingress: sandbox not found: x":
		t.Errorf("Grpc-Message = %q", message)
	case recorder.Header().Get("Retry-After") != "1":
		t.Errorf("Retry-After dropped")
	case recorder.Body.Len() != 0:
		t.Errorf("body = %q, want empty", recorder.Body.String())
	case sw.statusCode != http.StatusNotFound:
		t.Errorf("recorded status = %d, want 404 for metrics", sw.statusCode)
	}
}

func TestGRPCUnknownSandboxIsNotFound(t *testing.T) {
	backend := startGRPCBackend(t)
	client := dialIngress(t, startH2CIngress(t))

	_, err := client.Check(routedContext(t, "missing-sandbox-"+backend.port), &healthpb.HealthCheckRequest{})
	if status.Code(err) != codes.NotFound || !strings.Contains(status.Convert(err).Message(), "OpenSandbox Ingress") {
		t.Fatalf("err = %v; want NotFound from the ingress", err)
	}
}

func TestGRPCUpstreamDownIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	client := dialIngress(t, startH2CIngress(t))

	_, err = client.Check(routedContext(t, grpcTestSandbox+"-"+closedPort), &healthpb.HealthCheckRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v; want Unavailable", err)
	}
}

func TestWakeTimeoutForGRPCIsUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	err := &wakeGateError{target: sandbox.EndpointTarget{SandboxID: "sb"}, err: wake.ErrBudgetExhausted}

	if !respondWakeTimeout(recorder, r, err) {
		t.Fatal("respondWakeTimeout did not answer")
	}
	if recorder.Code != http.StatusOK || recorder.Header().Get("Grpc-Status") != "14" ||
		recorder.Header().Get(WakeMarkerHeader) != WakeMarkerExpired || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("got %d %v; want trailers-only UNAVAILABLE with wake marker", recorder.Code, recorder.Header())
	}
}
