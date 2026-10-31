// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"
)

func TestDockerExecOutputAndCancellation(t *testing.T) {
	for _, mode := range []string{"stream", "capture", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			attached := make(chan struct{})
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/container/exec"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"Id":"execution"}`)
				case strings.HasSuffix(r.URL.Path, "/exec/execution/start"):
					conn, output, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer close(closed)
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(output, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					_ = output.Flush()
					close(attached)
					if mode == "cancel" {
						_, _ = io.Copy(io.Discard, conn)
					} else {
						_, _ = io.Copy(stdcopy.NewStdWriter(conn, stdcopy.Stdout), strings.NewReader(strings.Repeat("x", 2*maxExecCaptureBytes)))
					}
				case strings.HasSuffix(r.URL.Path, "/exec/execution/json"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			apiClient, err := client.NewClientWithOpts(client.WithHost("tcp://"+server.Listener.Addr().String()), client.WithVersion("1.51"))
			require.NoError(t, err)
			defer func() { _ = apiClient.Close() }()
			engine := &Engine{client: apiClient, stdout: io.Discard, stderr: io.Discard}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if mode == "capture" {
					_, _, err = engine.Exec(ctx, "container", "root", "/", []string{"test"}, nil, nil)
				} else {
					err = engine.Run(ctx, "container", "root", "/", []string{"test"}, nil, nil)
				}
				done <- err
			}()
			select {
			case <-attached:
			case err := <-done:
				t.Fatalf("Docker execution failed before attachment: %v", err)
			case <-ctx.Done():
				t.Fatal("Docker attachment did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				switch mode {
				case "cancel":
					require.ErrorIs(t, err, context.Canceled)
				case "capture":
					require.ErrorContains(t, err, "output exceeds")
				default:
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Docker output read remained blocked")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Docker connection was not closed")
			}
		})
	}
}
