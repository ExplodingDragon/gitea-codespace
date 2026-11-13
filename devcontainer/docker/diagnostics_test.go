// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"
)

type diagnosticClient struct {
	client.APIClient
	tty  bool
	logs string
}

func (*diagnosticClient) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return []container.Summary{{ID: "primary"}, {ID: "sidecar"}}, nil
}

func (c *diagnosticClient) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{ExitCode: 255}}, Config: &container.Config{Tty: c.tty}}, nil
}

func (c *diagnosticClient) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	if c.tty {
		return io.NopCloser(strings.NewReader(c.logs)), nil
	}
	var output bytes.Buffer
	_, _ = io.WriteString(stdcopy.NewStdWriter(&output, stdcopy.Stdout), c.logs[:len(c.logs)/2])
	_, _ = io.WriteString(stdcopy.NewStdWriter(&output, stdcopy.Stderr), c.logs[len(c.logs)/2:])
	return io.NopCloser(&output), nil
}

func TestDiagnosticsLogTail(t *testing.T) {
	for _, tty := range []bool{false, true} {
		for _, large := range []bool{false, true} {
			name := "multiplexed"
			if tty {
				name = "tty"
			}
			if large {
				name += " limited"
			}
			t.Run(name, func(t *testing.T) {
				logs := "systemd failed\n"
				if large {
					logs += strings.Repeat("complete line\n", 3000) + strings.Repeat("partial-credential", 4000)
				}
				var output bytes.Buffer
				engine := &Engine{client: &diagnosticClient{tty: tty, logs: logs}, stderr: &output}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				engine.WriteDiagnostics(ctx, "owner")
				require.Contains(t, output.String(), "systemd failed\n")
				if large {
					require.Contains(t, output.String(), "64 KiB diagnostic limit")
					require.NotContains(t, output.String(), "partial-credential")
					require.Less(t, output.Len(), 65*1024)
				} else {
					require.Equal(t, 2, strings.Count(output.String(), "systemd failed"))
				}
			})
		}
	}
}
