// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package logbuffer batches diagnostic output within a fixed memory budget.
package logbuffer

import (
	"fmt"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const (
	MaxLineBytes  = 32 * 1024
	MaxBatchLines = 64
	MaxBatchBytes = 128 * 1024
	MaxBytes      = 1024 * 1024
	// LineOverheadBytes budgets the line record and queue entry separately
	// from its message, using the same accounting for queues and RPC batches.
	LineOverheadBytes = 64
)

// Buffer is owned by its caller's mutex. Bytes includes batches taken for upload
// until Release is called, so a blocked sender cannot bypass the memory limit.
type Buffer struct {
	lines        []*codespacev1.LogLine
	pendingBytes int
	bytes        int
	dropped      int
}

func (b *Buffer) Bytes() int    { return b.bytes }
func (b *Buffer) Pending() bool { return len(b.lines) > 0 }
func (b *Buffer) Ready() bool {
	return len(b.lines) >= MaxBatchLines || b.pendingBytes >= MaxBatchBytes
}
func (b *Buffer) Release(size int) { b.bytes -= size }
func (b *Buffer) Drop(count int)   { b.dropped += count }

// Add drops new lines when either the operation or caller's remaining budget is
// exhausted. A later successful append reports the aggregate omission once.
// The returned byte count lets the caller account for a shared queue budget.
func (b *Buffer) Add(lines []*codespacev1.LogLine, available int) int {
	available = min(available, MaxBytes-b.bytes)
	added := 0
	appendLine := func(line *codespacev1.LogLine) bool {
		size := len(line.Message) + LineOverheadBytes
		if size > available {
			return false
		}
		b.lines = append(b.lines, &codespacev1.LogLine{TimestampUnixNano: line.TimestampUnixNano, Message: line.Message})
		b.pendingBytes += size
		b.bytes += size
		available -= size
		added += size
		return true
	}
	if b.dropped > 0 && appendLine(&codespacev1.LogLine{TimestampUnixNano: time.Now().UnixNano(), Message: fmt.Sprintf("[WARN] [logging] Operation output omitted: %d lines lost", b.dropped)}) {
		b.dropped = 0
	}
	for _, line := range lines {
		if !appendLine(line) {
			b.dropped++
		}
	}
	return added
}

func (b *Buffer) Take() ([]*codespacev1.LogLine, int) {
	count, size := 0, 0
	for _, line := range b.lines {
		lineSize := len(line.Message) + LineOverheadBytes
		if count >= MaxBatchLines || (count > 0 && size+lineSize > MaxBatchBytes) {
			break
		}
		count++
		size += lineSize
	}
	lines := append([]*codespacev1.LogLine(nil), b.lines[:count]...)
	clear(b.lines[:count])
	b.lines = b.lines[count:]
	if len(b.lines) == 0 {
		b.lines = nil
	}
	b.pendingBytes -= size
	return lines, size
}
