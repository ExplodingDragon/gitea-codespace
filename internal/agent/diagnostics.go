// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"io"
	"strings"

	"gitea.dev/codespace/internal/cgroup"
	"golang.org/x/sys/unix"
)

func writeFailureDiagnostics(output io.Writer, reader cgroup.Reader, kata bool) {
	if output == nil {
		return
	}
	for _, name := range []string{"cgroup.type", "cgroup.subtree_control", "memory.events", "memory.current", "memory.max", "memory.peak"} {
		data, err := reader.Read(name)
		if err != nil {
			_, _ = fmt.Fprintf(output, "[DEBUG] [runtime] Cgroup %s unavailable: %v\n", name, err)
		} else {
			_, _ = fmt.Fprintf(output, "[DEBUG] [runtime] Cgroup %s: %s\n", name, strings.Join(strings.Fields(string(data)), " "))
		}
	}
	// Only Kata has a dedicated guest kernel. Host kernel logs from Native or
	// Sysbox could contain unrelated workloads and are not runtime diagnostics.
	if !kata {
		return
	}
	data := make([]byte, 64*1024)
	n, err := unix.Klogctl(unix.SYSLOG_ACTION_READ_ALL, data)
	if err != nil {
		_, _ = fmt.Fprintf(output, "[DEBUG] [runtime] Guest OOM messages unavailable: %v\n", err)
		return
	}
	lines := strings.Split(string(data[:n]), "\n")
	var messages []string
	for _, line := range lines[1:] {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "out of memory") || strings.Contains(lower, "oom") || strings.Contains(lower, "killed process") {
			messages = append(messages, line)
		}
	}
	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
	}
	_, _ = fmt.Fprintln(output, "[DEBUG] [runtime] Guest OOM messages (latest 20 matches in kernel log tail):")
	for _, message := range messages {
		_, _ = fmt.Fprintln(output, message)
	}
}
