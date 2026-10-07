// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const maxOutputLineBytes = 32 * 1024

var (
	logAuthorizationPattern  = regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]+`)
	logURLCredentialsPattern = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

// Output spools redacted output independently of its upload connection. stdout
// and stderr have separate partial-line buffers so their writes cannot interleave
// a secret across two otherwise unrelated lines.
type Output struct {
	log            *OperationLog
	replacements   *strings.Replacer
	stdout, stderr *outputWriter
	mu             sync.Mutex
	err            error
	truncated      bool
	pending        []*codespacev1.LogLine
	pendingBytes   int
	ready          chan struct{}
	pendingChanged chan struct{}
}

type outputWriter struct {
	output     *Output
	mu         sync.Mutex
	pending    []byte
	discarding bool
}

func NewOutput(log *OperationLog, credentials []string) *Output {
	values := make(map[string]struct{})
	for _, credential := range credentials {
		for _, value := range strings.FieldsFunc(credential, func(r rune) bool { return r == '\r' || r == '\n' }) {
			if value != "" {
				values[value] = struct{}{}
				values[url.QueryEscape(value)] = struct{}{}
				values[base64.StdEncoding.EncodeToString([]byte(value))] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(ordered))
	for _, value := range ordered {
		pairs = append(pairs, value, "[redacted]")
	}
	output := &Output{log: log, replacements: strings.NewReplacer(pairs...), ready: make(chan struct{}, 1), pendingChanged: make(chan struct{}, 1)}
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
			if len(w.pending)+len(part) > maxOutputLineBytes {
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
			w.output.append(strings.TrimSuffix(string(w.pending), "\r"))
		}
		w.pending = w.pending[:0]
		w.discarding = false
		data = data[end+1:]
	}
	// Logging failures must not close the child's output pipe or turn a
	// successful build into a failed operation. They remain visible via Err.
	return count, nil
}

func (o *Output) append(message string) {
	message = o.replacements.Replace(message)
	message = logAuthorizationPattern.ReplaceAllString(message, "${1}[redacted]")
	message = logURLCredentialsPattern.ReplaceAllString(message, "${1}[redacted]@")
	message = strings.ToValidUTF8(message, "\ufffd")
	if len(message) > maxOutputLineBytes {
		message = "[output line omitted: redacted line exceeds 32 KiB]"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil || o.truncated {
		return
	}
	o.pending = append(o.pending, &codespacev1.LogLine{TimestampUnixNano: time.Now().UnixNano(), Message: message})
	o.pendingBytes += len(message)
	select {
	case o.pendingChanged <- struct{}{}:
	default:
	}
	if len(o.pending) >= 64 || o.pendingBytes >= 128*1024 {
		o.flushPending()
	}
}

// flushPending is called with mu held; batches keep fsync independent of the
// number of progress lines emitted by a package manager or Docker build.
func (o *Output) flushPending() {
	if len(o.pending) == 0 || o.err != nil || o.truncated {
		return
	}
	err := o.log.Append(o.pending)
	o.pending, o.pendingBytes = nil, 0
	if errors.Is(err, ErrLogFull) {
		o.truncated = true
	} else if err != nil {
		o.err = fmt.Errorf("persist operation output: %w", err)
	} else {
		select {
		case o.ready <- struct{}{}:
		default:
		}
	}
}

func (o *Output) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// FlushLines is called only after all command writers have stopped. Uploads may
// continue concurrently because they read only committed batches.
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
	o.flushPending()
	o.mu.Unlock()
}

// Upload retries pending immutable batches until stopped. Callers join it before
// closing the spool, then attempt a bounded final Flush before reporting results.
func (o *Output) Upload(ctx context.Context, remote agentv1connect.AgentControlServiceClient) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.pendingChanged:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Second)
			continue
		case <-o.ready:
		case <-timer.C:
		}
		o.mu.Lock()
		o.flushPending()
		o.mu.Unlock()
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := o.log.Flush(attempt, remote)
		cancel()
		delay := 30 * time.Second
		if err != nil {
			delay = time.Second
		}
		timer.Reset(delay)
	}
}
