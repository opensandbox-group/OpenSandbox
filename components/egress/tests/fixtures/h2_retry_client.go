// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a standalone, synthetic GET-only observer, not a proxy replay client.
// Build this file alone with the explicitly approved exact Go version. It has
// no non-standard dependencies and never accepts an implicit version fallback.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	targetHost = "bound.example.com"
	maxBody    = 4 << 20
)

type output struct {
	mu     sync.Mutex
	writer *json.Encoder
	failed chan struct{}
	once   sync.Once
}

func (o *output) emit(event map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.writer.Encode(event) != nil {
		o.once.Do(func() { close(o.failed) })
	}
}

const maxH2DebugLine = 4096

var goAwayReadLog = regexp.MustCompile(`^http2: Framer 0x[0-9a-fA-F]+: read GOAWAY len=([0-9]+) LastStreamID=([0-9]+) ErrCode=([A-Z0-9_]+) Debug=`)

// filteredH2DebugWriter consumes Go 1.27.2 http2debug=2 diagnostics only in
// the explicit observer mode. It never forwards raw logger lines: DATA,
// headers, debug payloads and pointer identities are discarded in memory.
type filteredH2DebugWriter struct {
	mu       sync.Mutex
	line     []byte
	dropping bool
	out      *output
	started  time.Time
	ordinal  uint64
}

func newFilteredH2DebugWriter(out *output) *filteredH2DebugWriter {
	return &filteredH2DebugWriter{out: out, started: time.Now()}
}

func (w *filteredH2DebugWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range data {
		if b == '\n' {
			if w.dropping {
				w.out.emit(map[string]any{"event": "observer_log_line_overflow"})
			} else if len(w.line) != 0 {
				w.process(string(w.line))
			}
			clear(w.line)
			w.line = w.line[:0]
			w.dropping = false
			continue
		}
		if w.dropping {
			continue
		}
		if len(w.line) == maxH2DebugLine {
			clear(w.line)
			w.line = w.line[:0]
			w.dropping = true
			continue
		}
		w.line = append(w.line, b)
	}
	return len(data), nil
}

func (w *filteredH2DebugWriter) process(line string) {
	if !strings.HasPrefix(line, "http2: Framer ") || !strings.Contains(line, ": read GOAWAY ") {
		return
	}
	match := goAwayReadLog.FindStringSubmatch(line)
	if match == nil {
		w.out.emit(map[string]any{"event": "goaway_observer_invalid", "reason": "format"})
		return
	}
	frameLength, lengthErr := strconv.ParseUint(match[1], 10, 24)
	lastStreamID, lastErr := strconv.ParseUint(match[2], 10, 32)
	if lengthErr != nil || lastErr != nil || frameLength < 8 ||
		lastStreamID > 0x7fffffff || match[3] != "NO_ERROR" {
		w.out.emit(map[string]any{"event": "goaway_observer_invalid", "reason": "fields"})
		return
	}
	w.ordinal++
	w.out.emit(map[string]any{
		"event": "goaway_frame_parsed", "source": "go_http2debug_filtered_framer_read",
		"frame_type": "GOAWAY", "stream_id": 0,
		"last_stream_id": lastStreamID, "error_code": 0,
		"framer_ordinal": w.ordinal, "monotonic_elapsed_ns": time.Since(w.started).Nanoseconds(),
	})
}

// The TLS connection identities seen by GotConn are opaque sequential IDs.
// They are not stream IDs, transmission counts, or origin-processing counts.
type observations struct {
	mu      sync.Mutex
	ids     map[net.Conn]int
	counts  map[string]int
	writes  map[string]int
	used    map[string][]int
	raw     []net.Conn
	closing bool
}

func (o *observations) trackRaw(conn net.Conn) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing {
		_ = conn.Close()
		return context.Canceled
	}
	o.raw = append(o.raw, conn)
	return nil
}

func (o *observations) closeRaw() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closing = true
	for _, conn := range o.raw {
		_ = conn.Close()
	}
}

func (o *observations) trace(id string, out *output) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			o.mu.Lock()
			connectionID, exists := o.ids[info.Conn]
			if !exists {
				connectionID = len(o.ids) + 1
				o.ids[info.Conn] = connectionID
			}
			o.counts[id]++
			o.used[id] = append(o.used[id], connectionID)
			count := o.counts[id]
			o.mu.Unlock()
			out.emit(map[string]any{
				"event": "got_conn", "logical_id": id, "connection_id": connectionID,
				"got_conn_count": count, "reused": info.Reused, "was_idle": info.WasIdle,
			})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			o.mu.Lock()
			o.writes[id]++
			count := o.writes[id]
			o.mu.Unlock()
			out.emit(map[string]any{
				"event": "wrote_request", "logical_id": id, "write_count": count,
				"error_class": classify(info.Err),
			})
		},
	}
}

func (o *observations) snapshot(id string) (int, int, []int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[id], o.writes[id], append([]int{}, o.used[id]...)
}

// Only fixed classifications leave the process; never serialize error strings,
// response headers, certificates, CA paths, or body contents.
func classify(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "tls_verification_failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network_timeout"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "unexpected_eof"
	}
	return "transport_error"
}

// The actual origin destination is always 127.0.0.1:443. The supplied address
// selects only the loopback SOCKS5 endpoint, never another origin or port.
func socksDialer(address string, observed *observations) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		if (network != "tcp" && network != "tcp4") || target != targetHost+":443" {
			return nil, errors.New("unsupported_target")
		}
		dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp4", address)
		if err != nil {
			return nil, err
		}
		if err := observed.trackRaw(conn); err != nil {
			return nil, err
		}
		success := false
		defer func() {
			if !success {
				_ = conn.Close()
			}
		}()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		deadline := time.Now().Add(5 * time.Second)
		if expires, ok := ctx.Deadline(); ok && expires.Before(deadline) {
			deadline = expires
		}
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
			return nil, err
		}
		var greeting [2]byte
		if _, err := io.ReadFull(conn, greeting[:]); err != nil {
			return nil, err
		}
		if greeting != [2]byte{5, 0} {
			return nil, errors.New("socks_authentication_rejected")
		}
		// SOCKS5 CONNECT, IPv4 127.0.0.1, network-order port 443 (0x01bb).
		if _, err := conn.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, 1, 187}); err != nil {
			return nil, err
		}
		var reply [4]byte
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return nil, err
		}
		if reply[0] != 5 || reply[1] != 0 || reply[2] != 0 {
			return nil, errors.New("socks_connect_rejected")
		}
		var remaining int64
		switch reply[3] {
		case 1:
			remaining = 6
		case 4:
			remaining = 18
		case 3:
			var length [1]byte
			if _, err := io.ReadFull(conn, length[:]); err != nil {
				return nil, err
			}
			remaining = int64(length[0]) + 2
		default:
			return nil, errors.New("socks_invalid_reply")
		}
		if _, err := io.CopyN(io.Discard, conn, remaining); err != nil {
			return nil, err
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return nil, err
		}
		success = true
		return conn, nil
	}
}

type result struct {
	id    string
	error string
}

type requestState struct {
	headers  atomic.Bool
	finished atomic.Bool
}

func request(ctx context.Context, client *http.Client, id string, observed *observations, out *output, state *requestState, finished chan<- result) {
	ctx = httptrace.WithClientTrace(ctx, observed.trace(id, out))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+targetHost+"/v1/"+id, nil)
	if err != nil {
		panic("fixed synthetic request is invalid")
	}
	req.Header.Set("X-H2-Logical-ID", id)
	req.Header.Set("User-Agent", "OpenSandbox-H2-GET-observer/1")
	// Exactly one Do per logical request. Any further attempts belong solely to
	// this same net/http Transport; there is no manual retry or replacement.
	response, err := client.Do(req)
	status, protocol, negotiated, size := 0, "", "", int64(0)
	complete := false
	errorClass := classify(err)
	hash := sha256.New()
	if response != nil {
		status, protocol = response.StatusCode, response.Proto
		verified := false
		if response.TLS != nil {
			negotiated = response.TLS.NegotiatedProtocol
			verified = len(response.TLS.VerifiedChains) != 0
		}
		state.headers.Store(true)
		out.emit(map[string]any{
			"event": id + "_headers", "logical_id": id, "status": status,
			"proto": protocol, "negotiated_protocol": negotiated, "tls_verified": verified,
		})
		if id == "old" && (protocol != "HTTP/2.0" || negotiated != "h2") {
			errorClass = "old_response_not_h2"
		} else if err == nil {
			// This runs concurrently with the new GET and cannot leave a full
			// old-response window stalled while waiting for parent commands.
			size, err = io.Copy(hash, io.LimitReader(response.Body, maxBody+1))
			errorClass = classify(err)
			if size > maxBody {
				errorClass = "body_limit_exceeded"
			}
			complete = errorClass == ""
		}
		_ = response.Body.Close()
	}
	count, writes, connections := observed.snapshot(id)
	state.finished.Store(true)
	out.emit(map[string]any{
		"event": id + "_result", "logical_id": id, "do_calls": 1,
		"status": status, "proto": protocol, "negotiated_protocol": negotiated,
		"body_bytes": size, "body_sha256": hex.EncodeToString(hash.Sum(nil)),
		"body_complete": complete, "error_class": errorClass,
		"got_conn_count": count, "write_count": writes, "connection_ids": connections,
	})
	finished <- result{id: id, error: errorClass}
}

func controls(ctx context.Context, commands chan<- string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		var command struct {
			Command string `json:"command"`
		}
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		name := "invalid_control"
		if decoder.Decode(&command) == nil && decoder.Decode(new(any)) == io.EOF &&
			(command.Command == "continue_new" || command.Command == "cancel") {
			name = command.Command
		}
		select {
		case commands <- name:
		case <-ctx.Done():
			return
		}
		if name == "invalid_control" || name == "cancel" {
			return
		}
	}
	name := "stdin_eof"
	if scanner.Err() != nil {
		name = "stdin_error"
	}
	select {
	case commands <- name:
	case <-ctx.Done():
	}
}

func goAwayObserverModeAllowed(enabled bool, debugSetting string) bool {
	if enabled {
		return debugSetting == "http2debug=2"
	}
	return debugSetting == ""
}

func run() int {
	out := &output{writer: json.NewEncoder(os.Stdout), failed: make(chan struct{})}
	requiredVersion := ""
	fail := func(reason string) int {
		out.emit(map[string]any{
			"event": "fatal", "error_class": reason,
			"go_version": runtime.Version(), "required_go_version": requiredVersion,
		})
		return 2
	}
	flags := flag.NewFlagSet("h2_retry_client", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&requiredVersion, "require-go-version", "", "required exact approved stable Go runtime version")
	observeGoAway := flags.Bool("observe-goaway-parse", false, "filter exact Go 1.27.2 HTTP/2 parser logs into sanitized events")
	socks := flags.String("socks-address", "", "IPv4 loopback SOCKS5 endpoint")
	caFile := flags.String("ca-file", "", "combined synthetic CA PEM file")
	timeout := flags.Duration("timeout", 25*time.Second, "overall deadline")
	newTimeout := flags.Duration("new-timeout", 8*time.Second, "new GET deadline, independent of old GET")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 {
		return fail("invalid_arguments")
	}
	if !regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(requiredVersion) {
		return fail("required_go_version_missing_or_invalid")
	}
	if runtime.Version() != requiredVersion {
		return fail("unsupported_go_version")
	}
	debugSetting := os.Getenv("GODEBUG")
	if !goAwayObserverModeAllowed(*observeGoAway, debugSetting) {
		if *observeGoAway {
			return fail("goaway_observer_requires_exact_godebug")
		}
		return fail("ambient_godebug_not_supported")
	}
	if *observeGoAway {
		log.SetFlags(0)
		log.SetOutput(newFilteredH2DebugWriter(out))
	} else if debugSetting != "" {
		return fail("ambient_godebug_not_supported")
	}
	host, port, err := net.SplitHostPort(*socks)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || host != "127.0.0.1" || portErr != nil || portNumber == 0 || *caFile == "" ||
		*timeout < time.Second || *timeout > 2*time.Minute || *newTimeout < 100*time.Millisecond || *newTimeout >= *timeout {
		return fail("invalid_arguments")
	}
	pem, err := os.ReadFile(*caFile)
	if err != nil || len(pem) > 1<<20 {
		return fail("ca_file_unreadable_or_oversized")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fail("ca_file_invalid")
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	defer os.Stdin.Close()
	observed := &observations{
		ids: make(map[net.Conn]int), counts: make(map[string]int),
		writes: make(map[string]int), used: make(map[string][]int),
	}
	defer observed.closeRaw()
	transport := &http.Transport{
		Proxy: nil, DialContext: socksDialer(*socks, observed),
		TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: targetHost, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true, DisableCompression: true,
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 10 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	out.emit(map[string]any{
		"event": "ready", "schema": 1, "go_version": runtime.Version(), "required_go_version": requiredVersion,
		"transport": map[string]any{
			"force_attempt_http2": true, "proxy_environment": false, "dial": "loopback_socks5_ipv4",
			"goaway_observer_mode": map[bool]string{true: "filtered_go_http2debug_parser_event", false: "disabled"}[*observeGoAway],
			"origin":               "127.0.0.1:443", "authority": targetHost, "tls_server_name": targetHost,
			"insecure_skip_verify": false, "trust": "supplied_ca_only", "tls_min_version": "1.2",
			"disable_compression": true, "redirects": false, "manual_retries": 0,
			"max_idle_conns": 2, "max_idle_conns_per_host": 2, "max_conns_per_host": 0,
			"idle_conn_timeout_ms": 10000, "tls_handshake_timeout_ms": 5000,
			"dial_timeout_ms": 5000, "socks_handshake_timeout_ms": 5000,
			"overall_timeout_ms": timeout.Milliseconds(), "new_timeout_ms": newTimeout.Milliseconds(),
			"max_body_bytes": maxBody,
		},
	})
	commands, finished := make(chan string, 1), make(chan result, 2)
	go controls(ctx, commands)
	var oldState, newState requestState
	go request(ctx, client, "old", observed, out, &oldState, finished)
	pending, newStarted, oldError, newError, failure := 1, false, "", "", ""
	done := ctx.Done()
	for pending != 0 || (!newStarted && failure == "") {
		select {
		case command := <-commands:
			if command == "continue_new" && oldState.headers.Load() && !oldState.finished.Load() && !newStarted && failure == "" {
				newStarted = true
				pending++
				go func() {
					newCtx, stop := context.WithTimeout(ctx, *newTimeout)
					defer stop()
					request(newCtx, client, "new", observed, out, &newState, finished)
				}()
			} else {
				if command == "continue_new" {
					command = "invalid_control_order"
				}
				failure = command
				cancel()
			}
		case received := <-finished:
			pending--
			if received.id == "old" {
				oldError = received.error
				if oldError != "" {
					failure = "old_request_failed"
					cancel()
				} else if !newStarted {
					failure = "old_completed_before_continue"
					cancel()
				}
			} else {
				newError = received.error
			}
		case <-done:
			if failure == "" {
				failure = classify(ctx.Err())
			}
			// Closing every owned connection also bounds unfinished SOCKS/TLS
			// work. A new-request deadline alone never reaches this branch.
			observed.closeRaw()
			done = nil
		case <-out.failed:
			cancel()
			observed.closeRaw()
			return 2
		}
	}
	outcome := "observed"
	code := 0
	if failure != "" || !newStarted || oldError != "" {
		outcome, code = "incomplete", 2
	}
	// Exit 0 means both observations completed with the old response intact.
	// The parent must independently judge retry outcome and origin at-most-once.
	out.emit(map[string]any{
		"event": "done", "outcome": outcome, "error_class": failure,
		"old_error_class": oldError, "new_error_class": newError,
		"new_started": newStarted, "exit_code": code,
	})
	return code
}

func main() { os.Exit(run()) }
