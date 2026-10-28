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

// Regression tests for file transfers: an upload or download that takes longer
// than the client's overall request timeout must still complete, while the
// wait for response headers stays bounded.

package opensandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowReader yields its chunks with a pause before each one.
type slowReader struct {
	chunks []string
	delay  time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestDownloadFile_NotKilledByRequestTimeout(t *testing.T) {
	// The body arrives over ~600ms, well beyond the 200ms request timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = w.Write([]byte("chunk"))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(120 * time.Millisecond)
		}
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithTimeout(200*time.Millisecond))
	body, err := client.DownloadFile(context.Background(), "/data/big.bin", "")
	require.NoError(t, err)
	defer body.Close()

	data, err := io.ReadAll(body)
	require.NoError(t, err, "a download must not be cut off by the overall request timeout")
	require.Equal(t, strings.Repeat("chunk", 5), string(data))
}

func TestUploadFile_NotKilledByRequestTimeout(t *testing.T) {
	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received = len(data)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The request body takes ~600ms to send, well beyond the 200ms request timeout.
	file := &slowReader{chunks: []string{"a", "b", "c", "d", "e"}, delay: 120 * time.Millisecond}
	client := NewExecdClient(srv.URL, "tok", WithTimeout(200*time.Millisecond))
	err := client.UploadFile(context.Background(), file, UploadFileOptions{Metadata: FileMetadata{Path: "/data/big.bin"}})
	require.NoError(t, err, "an upload must not be cut off by the overall request timeout")
	require.True(t, received > 0, "the server should have received the upload")
}

func TestDownloadFile_HeaderWaitBounded(t *testing.T) {
	// The server accepts the request but never sends response headers.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	tr := DefaultTransport()
	tr.ResponseHeaderTimeout = 200 * time.Millisecond
	client := NewExecdClient(srv.URL, "tok", WithHTTPClient(&http.Client{Transport: tr}))

	done := make(chan error, 1)
	go func() {
		body, err := client.DownloadFile(context.Background(), "/data/big.bin", "")
		if body != nil {
			body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a download must fail when the server never sends response headers")
	case <-time.After(5 * time.Second):
		t.Fatal("download hung waiting for response headers")
	}
}

func TestTransferClient_KeepsPoolingAndClearsTimeout(t *testing.T) {
	client := NewExecdClient("https://example.com", "tok", WithTimeout(30*time.Second))
	tc := client.client.transferHTTPClient()
	require.Equal(t, time.Duration(0), tc.Timeout, "transfer client must have no overall timeout")
	tr, ok := tc.Transport.(*http.Transport)
	require.True(t, ok, "transfer transport should be *http.Transport")
	require.True(t, !tr.DisableKeepAlives, "transfers keep connection pooling")
	require.Equal(t, streamResponseHeaderTimeout, tr.ResponseHeaderTimeout,
		"transfer client must bound the wait for response headers")

	// The normal client keeps its overall timeout.
	require.Equal(t, 30*time.Second, client.client.httpClient.Timeout, "non-transfer client keeps its request timeout")
}
