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

//go:build linux

package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func serveFilesystemIdentity(c *gin.Context, uid, gid uint32) {
	fail := func(err error) {
		c.JSON(http.StatusServiceUnavailable, model.ErrorResponse{
			Code: "FILESYSTEM_IDENTITY_UNAVAILABLE", Message: err.Error(),
		})
	}
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fail(err)
		return
	}
	parent := os.NewFile(uintptr(sockets[0]), "filesystem-parent")
	child := os.NewFile(uintptr(sockets[1]), "filesystem-child")
	defer child.Close()
	conn, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		fail(err)
		return
	}
	defer conn.Close()
	// Re-exec the currently running inode, rather than resolving an executable
	// through a workload-controlled PATH or a replaceable filesystem path.
	cmd := exec.Command("/proc/self/exe", FilesystemWorkerArg, c.Param("uid"), c.Param("gid"))
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stderr = os.Stderr
	stop, err := runtime.StartFilesystemWorker(cmd, uid, gid)
	if err != nil {
		fail(err)
		return
	}
	defer stop()
	_ = child.Close()

	transport := &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext:        func(context.Context, string, string) (net.Conn, error) { return conn, nil },
	}
	defer transport.CloseIdleConnections()
	prefix := "/v1/filesystem/" + c.Param("uid") + "/" + c.Param("gid")
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "filesystem-worker"
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			r.URL.RawPath = ""
			r.Header.Del(model.ApiAccessTokenHeader)
			r.Header.Del("Authorization")
			r.Header.Del("Cookie")
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler:  func(_ http.ResponseWriter, _ *http.Request, err error) { fail(err) },
	}
	// Supply cancellation even when this handler is embedded without a native
	// net/http server context; avoid the deprecated CloseNotifier fallback.
	requestContext, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	proxy.ServeHTTP(c.Writer, c.Request.WithContext(requestContext))
}

// RunFilesystemWorker serves one HTTP request over inherited socket fd 3.
// There is no listening network endpoint and no command/proxy route.
func RunFilesystemWorker() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("invalid filesystem worker arguments")
	}
	uid, err := parseFilesystemID(os.Args[2])
	if err != nil {
		return err
	}
	gid, err := parseFilesystemID(os.Args[3])
	if err != nil {
		return err
	}
	if uint32(os.Getuid()) != uid || uint32(os.Geteuid()) != uid ||
		uint32(os.Getgid()) != gid || uint32(os.Getegid()) != gid {
		return fmt.Errorf("filesystem worker identity does not match requested uid/gid")
	}
	// Reject inherited capabilities rather than dropping them on just one Go
	// thread: Linux capabilities are per-thread, and HTTP handlers may run on
	// any runtime thread. A normal credential-changing exec clears these.
	if uid != 0 {
		hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		data := [2]unix.CapUserData{}
		if err := unix.Capget(&hdr, &data[0]); err != nil {
			return err
		}
		for _, capabilities := range data {
			if capabilities.Effective != 0 || capabilities.Permitted != 0 || capabilities.Inheritable != 0 {
				return fmt.Errorf("unprivileged filesystem worker inherited capabilities")
			}
		}
	}
	file := os.NewFile(3, "filesystem-worker")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	listener := &filesystemWorkerListener{conn: conn, done: make(chan struct{})}
	defer listener.Close()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	registerFilesystemRoutes(router.Group(""))
	server := &http.Server{Handler: router}
	server.SetKeepAlivesEnabled(false)
	if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Accept returns the inherited connection once, then waits for it to close.
// Waiting prevents Serve from returning before the response has been flushed.
type filesystemWorkerListener struct {
	conn     net.Conn
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *filesystemWorkerListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return &filesystemWorkerConn{Conn: l.conn, listener: l}, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *filesystemWorkerListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.conn.Close()
}

func (l *filesystemWorkerListener) Addr() net.Addr { return l.conn.LocalAddr() }

type filesystemWorkerConn struct {
	net.Conn
	listener *filesystemWorkerListener
}

func (c *filesystemWorkerConn) Close() error { return c.listener.Close() }
