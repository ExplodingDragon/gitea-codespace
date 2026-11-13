// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
)

// WriteDiagnostics records container state before rollback removes the evidence.
// Container metadata excludes environment, labels, mounts and command arguments.
// Logs use the caller's redacting writer, just like lifecycle command output.
func (e *Engine) WriteDiagnostics(ctx context.Context, ownerID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	query := filters.NewArgs(filters.Arg("label", labelOwnerID+"="+ownerID))
	containers, err := e.client.ContainerList(ctx, container.ListOptions{All: true, Filters: query})
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] Dev Container diagnostics unavailable: %v\n", err)
		return
	}
	remaining := 64 * 1024
	for _, item := range containers {
		inspect, err := e.client.ContainerInspect(ctx, item.ID)
		if err != nil || inspect.State == nil {
			_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] container=%s state unavailable: %v\n", item.ID, err)
			continue
		}
		data, err := json.Marshal(struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			Running    bool   `json:"running"`
			OOMKilled  bool   `json:"oomKilled"`
			ExitCode   int    `json:"exitCode"`
			Error      string `json:"error"`
			StartedAt  string `json:"startedAt"`
			FinishedAt string `json:"finishedAt"`
		}{ID: item.ID, Status: inspect.State.Status, Running: inspect.State.Running, OOMKilled: inspect.State.OOMKilled, ExitCode: inspect.State.ExitCode, Error: inspect.State.Error, StartedAt: inspect.State.StartedAt, FinishedAt: inspect.State.FinishedAt})
		if err == nil {
			_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] Dev Container %s\n", data)
		}
		if ctx.Err() != nil {
			break
		}
		if remaining == 0 {
			continue
		}
		logs, err := e.client.ContainerLogs(ctx, item.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "200"})
		if err != nil {
			_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] container=%s logs unavailable: %v\n", item.ID, err)
			continue
		}
		captured := diagnosticLogBuffer{limit: remaining + 1}
		if inspect.Config != nil && inspect.Config.Tty {
			_, err = io.Copy(&captured, logs)
		} else {
			_, err = stdcopy.StdCopy(&captured, &captured, logs)
		}
		_ = logs.Close()
		data = captured.data
		truncated := len(data) > remaining
		if truncated {
			// Emit whole lines only: a partial credential at the byte boundary
			// would no longer match the caller's redaction rules.
			data = data[:bytes.LastIndexByte(data[:remaining], '\n')+1]
			remaining = 0
		} else {
			remaining -= len(data)
			if err != nil {
				data = data[:bytes.LastIndexByte(data, '\n')+1]
			}
		}
		_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] container=%s log tail:\n", item.ID)
		_, _ = e.stderr.Write(data)
		_, _ = fmt.Fprintln(e.stderr)
		if truncated {
			_, _ = fmt.Fprintln(e.stderr, "[DEBUG] [container] Log output reached the 64 KiB diagnostic limit")
		} else if err != nil {
			_, _ = fmt.Fprintf(e.stderr, "[DEBUG] [container] container=%s read logs: %v\n", item.ID, err)
		}
	}
}

type diagnosticLogBuffer struct {
	data  []byte
	limit int
}

func (b *diagnosticLogBuffer) Write(data []byte) (int, error) {
	n := min(len(data), b.limit-len(b.data))
	b.data = append(b.data, data[:n]...)
	if n != len(data) {
		return n, errors.New("container diagnostic log limit reached")
	}
	return n, nil
}
