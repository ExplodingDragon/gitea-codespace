// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	maxLogBatchBytes = 256 * 1024
	maxLogFileBytes  = 32 * 1024 * 1024
)

// ErrLogFull means further output is deliberately discarded, not execution failure.
var ErrLogFull = errors.New("operation log spool is full")

// PruneLogs removes retired operation spools after their writers have joined.
// A newer Manager operation closes the previous Gitea log window, so retaining
// those batches could only grow the PVC without a possible upload destination.
func (j *Journal) PruneLogs(version int64) error {
	if version <= 0 {
		return fmt.Errorf("invalid current log operation")
	}
	directory, err := j.root.OpenFile("logs", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name, suffix, ok := strings.Cut(entry.Name(), ".")
		previous, err := strconv.ParseInt(name, 10, 64)
		if !ok || err != nil || previous <= 0 || previous >= version || (suffix != "pb" && suffix != "json") || entry.IsDir() {
			continue
		}
		if err := j.root.Remove("logs/" + entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return directory.Sync()
}

type logCheckpoint struct {
	Position int64 `json:"position"`
	Offset   int64 `json:"offset"`
	Closed   bool  `json:"closed"`
}

// OperationLog stores immutable batches before sending them. The remote byte
// offset is separate from the local protobuf position and advances only on ack.
type OperationLog struct {
	mu             sync.Mutex
	flushMu        sync.Mutex
	journal        *Journal
	file           *os.File
	version        int64
	checkpointPath string
	checkpoint     logCheckpoint
	size           int64
	full           bool
}

func OpenOperationLog(journal *Journal, version, initialOffset int64) (_ *OperationLog, err error) {
	if journal == nil || version <= 0 || initialOffset < 0 {
		return nil, fmt.Errorf("invalid operation log identity")
	}
	name := fmt.Sprintf("logs/%d", version)
	log := &OperationLog{journal: journal, version: version, checkpointPath: name + ".json"}
	log.file, err = journal.root.OpenFile(name+".pb", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = log.file.Close()
		}
	}()
	info, err := log.file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxLogFileBytes {
		return nil, fmt.Errorf("invalid operation log file")
	}
	// The Journal owns the volume lock. Also prevent two sinks from advancing
	// the same operation's checkpoint within that process.
	if err := unix.Flock(int(log.file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("operation log already has a writer: %w", err)
	}
	log.size = info.Size()
	data, err := journal.read(log.checkpointPath, 4096)
	if errors.Is(err, os.ErrNotExist) {
		if log.size != 0 {
			return nil, fmt.Errorf("operation log has no offset checkpoint")
		}
		log.checkpoint.Offset = initialOffset
		data, err = json.Marshal(log.checkpoint)
		if err == nil {
			err = journal.write(log.checkpointPath, data)
		}
	} else if err == nil {
		err = json.Unmarshal(data, &log.checkpoint)
	}
	if err != nil {
		return nil, err
	}
	if log.checkpoint.Position < 0 || log.checkpoint.Position > log.size || log.checkpoint.Offset < 0 {
		return nil, fmt.Errorf("invalid operation log checkpoint")
	}
	return log, nil
}

func (l *OperationLog) Close() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// Append accepts lines after credential redaction. It never stores access bundles.
func (l *OperationLog) Append(lines []*codespacev1.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	batch := &agentv1.UploadLogsRequest{ProtocolVersion: 1, OperationRversion: l.version, Lines: lines}
	if proto.Size(batch) > maxLogBatchBytes {
		return fmt.Errorf("operation log batch exceeds the size limit")
	}
	for _, line := range lines {
		if line == nil || line.TimestampUnixNano <= 0 {
			return fmt.Errorf("invalid operation log line")
		}
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		return err
	}
	record := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(record, uint32(len(data)))
	copy(record[4:], data)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.checkpoint.Closed {
		return nil
	}
	if l.full {
		return ErrLogFull
	}
	if l.size+int64(len(record)) > maxLogFileBytes-1024 {
		batch.Lines = []*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: "[operation output truncated: local log capacity reached]"}}
		data, err = proto.Marshal(batch)
		if err != nil {
			return err
		}
		record = make([]byte, 4+len(data))
		binary.BigEndian.PutUint32(record, uint32(len(data)))
		copy(record[4:], data)
		if l.size+int64(len(record)) > maxLogFileBytes {
			l.full = true
			return ErrLogFull
		}
		l.full = true
	}
	if _, err := l.file.WriteAt(record, l.size); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	l.size += int64(len(record))
	if l.full {
		return ErrLogFull
	}
	return nil
}

// Flush uploads the saved prefix. A lost reply leaves both the batch and offset
// unchanged, allowing Gitea's byte-for-byte replay check to acknowledge it later.
func (l *OperationLog) Flush(ctx context.Context, remote agentv1connect.AgentControlServiceClient) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	for {
		l.mu.Lock()
		checkpoint, fileSize := l.checkpoint, l.size
		l.mu.Unlock()
		if checkpoint.Closed || checkpoint.Position == fileSize {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var header [4]byte
		if _, err := l.file.ReadAt(header[:], checkpoint.Position); err != nil {
			return fmt.Errorf("read operation log batch header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[:]))
		if size == 0 || size > maxLogBatchBytes || size > fileSize-checkpoint.Position-4 {
			return fmt.Errorf("operation log batch is incomplete or corrupt")
		}
		data := make([]byte, size)
		if _, err := l.file.ReadAt(data, checkpoint.Position+4); err != nil {
			return err
		}
		batch := &agentv1.UploadLogsRequest{}
		if err := proto.Unmarshal(data, batch); err != nil {
			return err
		}
		if batch.ProtocolVersion != 1 || batch.OperationRversion != l.version || len(batch.Lines) == 0 || batch.Offset != 0 {
			return fmt.Errorf("operation log batch identity changed")
		}
		batch.Offset = checkpoint.Offset
		response, err := remote.UploadLogs(ctx, connect.NewRequest(batch))
		if err != nil {
			return err
		}
		if response.Msg.NextOffset < checkpoint.Offset || (!response.Msg.Closed && response.Msg.NextOffset == checkpoint.Offset) || (response.Msg.Closed && response.Msg.NextOffset != checkpoint.Offset) {
			return fmt.Errorf("invalid operation log acknowledgement")
		}
		next := logCheckpoint{Position: checkpoint.Position + 4 + size, Offset: response.Msg.NextOffset, Closed: response.Msg.Closed}
		data, err = json.Marshal(next)
		if err != nil {
			return err
		}
		if err := l.journal.write(l.checkpointPath, data); err != nil {
			return err
		}
		l.mu.Lock()
		l.checkpoint = next
		l.mu.Unlock()
	}
}
