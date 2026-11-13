// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/logbuffer"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
)

// Output buffers redacted output independently of its upload connection. stdout
// and stderr have separate partial-line buffers so their writes cannot interleave
// a secret across two otherwise unrelated lines.
type Output struct {
	version        int64
	redactor       *logbuffer.Redactor
	stdout, stderr *outputWriter
	mu             sync.Mutex
	buffer         logbuffer.Buffer
	changed        chan struct{}
	finishing      bool
	closed         bool
}

type outputWriter struct {
	output     *Output
	mu         sync.Mutex
	pending    []byte
	discarding bool
}

func NewOutput(version int64, credentials []string) *Output {
	output := &Output{version: version, redactor: logbuffer.NewRedactor(credentials), changed: make(chan struct{}, 1)}
	output.stdout, output.stderr = &outputWriter{output: output}, &outputWriter{output: output}
	return output
}

func (o *Output) Stdout() io.Writer { return o.stdout }
func (o *Output) Stderr() io.Writer { return o.stderr }

func (w *outputWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !w.discarding {
			if len(w.pending)+len(part) > logbuffer.MaxLineBytes {
				// Truncating a prefix could leak a secret split at the boundary.
				w.pending = nil
				w.discarding = true
				w.output.append("[output line omitted: exceeds 32 KiB]")
			} else {
				w.pending = append(w.pending, part...)
			}
		}
		if end < 0 {
			break
		}
		if !w.discarding {
			w.output.append(string(w.pending))
		}
		w.pending = w.pending[:0]
		w.discarding = false
		data = data[end+1:]
	}
	// Logging failures must not close the child's output pipe.
	return count, nil
}

func (o *Output) append(message string) {
	message = o.redactor.Redact(message)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.buffer.Add([]*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: message}}, logbuffer.MaxBytes)
	select {
	case o.changed <- struct{}{}:
	default:
	}
}

// FlushLines is called only after all command writers have stopped. Uploads may
// continue concurrently because partial lines are separate from queued batches.
func (o *Output) FlushLines() {
	for _, writer := range []*outputWriter{o.stdout, o.stderr} {
		writer.mu.Lock()
		if len(writer.pending) != 0 && !writer.discarding {
			o.append(string(writer.pending))
		}
		writer.pending = nil
		writer.mu.Unlock()
	}
	o.mu.Lock()
	o.buffer.Add(nil, logbuffer.MaxBytes)
	o.finishing = true
	o.mu.Unlock()
	select {
	case o.changed <- struct{}{}:
	default:
	}
}

// Upload submits each batch once: an interrupted reply may already have been
// accepted by Manager. Later batches can proceed without replaying that output.
func (o *Output) Upload(ctx context.Context, remote agentv1connect.AgentControlServiceClient) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	timer.Stop()
	var deadline <-chan time.Time
	defer func() {
		o.mu.Lock()
		o.closed = true
		o.buffer = logbuffer.Buffer{}
		o.mu.Unlock()
	}()
	for {
		o.mu.Lock()
		pending, ready, finishing := o.buffer.Pending(), o.buffer.Ready(), o.finishing
		o.mu.Unlock()
		if finishing && !pending {
			return
		}
		if pending && deadline == nil {
			timer.Reset(time.Second)
			deadline = timer.C
		}
		if !pending || (!ready && !finishing) {
			select {
			case <-ctx.Done():
				return
			case <-o.changed:
				continue
			case <-deadline:
			}
		}
		if ctx.Err() != nil {
			return
		}
		timer.Stop()
		deadline = nil
		o.mu.Lock()
		lines, size := o.buffer.Take()
		o.mu.Unlock()
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		response, err := remote.UploadLogs(attempt, &agentv1.UploadLogsRequest{ProtocolVersion: 1, OperationRversion: o.version, Lines: lines})
		cancel()
		o.mu.Lock()
		o.buffer.Release(size)
		if err != nil {
			o.buffer.Drop(len(lines))
		}
		o.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Upload Agent operation output", "operation", o.version, "error", err)
		}
		if response.GetClosed() {
			return
		}
	}
}
