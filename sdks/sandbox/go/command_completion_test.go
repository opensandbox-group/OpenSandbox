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

package opensandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestRunCommandForegroundStopsAtAuthoritativeExit(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"execution_complete","exit_code":0}`,
		`{"type":"error","error":{"ename":"CommandExecError","evalue":"7","exit_code":7}}`,
	} {
		for _, callbackFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/callbackFails=%v", terminal, callbackFails), func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				closed := make(chan struct{})
				_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = fmt.Fprint(w, "data: {\"type\":\"stdout\",\"text\":\"tail\"}\n\ndata: "+terminal+"\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						close(closed)
					case <-release:
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				callbackErr := fmt.Errorf("terminal callback failed")
				var got []string
				err := client.RunCommand(ctx, RunCommandRequest{Command: "echo tail"}, func(event StreamEvent) error {
					got = append(got, event.Data)
					if event.Data == terminal && callbackFails {
						return callbackErr
					}
					return nil
				})
				if callbackFails {
					require.ErrorIs(t, err, callbackErr)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, []string{`{"type":"stdout","text":"tail"}`, terminal}, got)
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("response was not closed")
				}
			})
		}
	}
}

func TestRunCommandForegroundNullExitCodeDrainsTrailingOutput(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"execution_complete","exit_code":null}`,
		`{"type":"error","error":{"ename":"CommandExecError","evalue":"7","exit_code":null}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			frames := []string{terminal, `{"type":"stdout","text":"late output"}`}
			_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
				for _, frame := range frames {
					_, _ = fmt.Fprint(w, "data: "+frame+"\n\n")
				}
			})
			var got []string
			err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo test"}, func(event StreamEvent) error {
				got = append(got, event.Data)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, frames, got)
		})
	}
}
