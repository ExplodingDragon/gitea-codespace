// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"gitea.dev/codespace/internal/provisioner"
)

const (
	gatewayWebSSHInputLimit = 64 * 1024
	gatewayWebSSHWriteTime  = 10 * time.Second
)

type gatewayWorkspaceTerminal struct {
	state   gatewayWorkspaceTargetStore
	backend gatewayWorkspaceCommandBackend
}

type gatewayWebSSHControlMessage struct {
	Type     string `json:"type"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Code     int    `json:"code,omitempty"`
	Category string `json:"category,omitempty"`
}

func newGatewayWorkspaceTerminal(state gatewayWorkspaceTargetStore, backend gatewayWorkspaceCommandBackend) *gatewayWorkspaceTerminal {
	return &gatewayWorkspaceTerminal{
		state:   state,
		backend: backend,
	}
}

func (t *gatewayWorkspaceTerminal) ServeHTTP(writer http.ResponseWriter, request *http.Request, codespaceUUID, upstreamPath string) {
	if t == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "gateway web ssh is not ready"})
		return
	}
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch upstreamPath {
	case "/":
		serveGatewayWebSSHPage(writer, request)
	case "/.gitea-codespace/assets/terminal.js":
		serveGatewayWebSSHScript(writer)
	case "/.gitea-codespace/assets/terminal.css":
		serveGatewayWebSSHStyle(writer)
	case "/.gitea-codespace/terminal":
		t.serveTerminal(writer, request, codespaceUUID)
	default:
		http.NotFound(writer, request)
	}
}

func serveGatewayWebSSHPage(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	assetBase := "./.gitea-codespace/assets"
	if request != nil && request.URL.Path == "/w/" {
		assetBase = "/.gitea-codespace/assets"
	}
	_, _ = fmt.Fprintf(writer, gatewayWebSSHPage, assetBase, assetBase)
}

func serveGatewayWebSSHScript(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHScript)
}

func serveGatewayWebSSHStyle(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/css; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHStyle)
}

func (t *gatewayWorkspaceTerminal) serveTerminal(writer http.ResponseWriter, request *http.Request, codespaceUUID string) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	session, err := t.openSession(request.Context(), codespaceUUID)
	if err != nil {
		_ = writeGatewayWebSSHControl(conn, gatewayWebSSHControlMessage{Type: "error", Category: gatewayWebSSHErrorCategory(err)})
		return
	}
	defer session.Close()
	if err := request.Context().Err(); err != nil {
		return
	}

	if err := session.Resize(120, 40); err != nil {
		_ = writeGatewayWebSSHControl(conn, gatewayWebSSHControlMessage{Type: "error", Category: "runtime_unavailable"})
		return
	}
	writerLock := &sync.Mutex{}
	if err := writeGatewayWebSSHControlLocked(conn, writerLock, gatewayWebSSHControlMessage{Type: "ready"}); err != nil {
		return
	}

	outputDone := make(chan struct{}, 1)
	go func() {
		t.copyOutput(conn, writerLock, session.Stdout())
		outputDone <- struct{}{}
	}()
	stderrDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(io.Discard, session.Stderr())
		stderrDone <- struct{}{}
	}()
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- session.Wait()
	}()
	go func() {
		select {
		case <-request.Context().Done():
			_ = conn.Close()
		case err := <-waitDone:
			waitGatewayWebSSHOutput(outputDone)
			waitGatewayWebSSHOutput(stderrDone)
			code := gatewayWebSSHExitCode(err)
			_ = writeGatewayWebSSHControlLocked(conn, writerLock, gatewayWebSSHControlMessage{Type: "exit", Code: code})
			_ = conn.Close()
		}
	}()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch messageType {
		case websocket.BinaryMessage:
			if len(message) > gatewayWebSSHInputLimit {
				_ = writeGatewayWebSSHControlLocked(conn, writerLock, gatewayWebSSHControlMessage{Type: "error", Category: "protocol_error"})
				return
			}
			if _, err := session.Stdin().Write(message); err != nil {
				return
			}
		case websocket.TextMessage:
			if err := handleGatewayWebSSHControlInput(session, message); err != nil {
				_ = writeGatewayWebSSHControlLocked(conn, writerLock, gatewayWebSSHControlMessage{Type: "error", Category: "protocol_error"})
				return
			}
		default:
			_ = writeGatewayWebSSHControlLocked(conn, writerLock, gatewayWebSSHControlMessage{Type: "error", Category: "protocol_error"})
			return
		}
	}
}

func waitGatewayWebSSHOutput(done <-chan struct{}) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (t *gatewayWorkspaceTerminal) openSession(ctx context.Context, codespaceUUID string) (provisioner.WorkspaceCommandSession, error) {
	if t.state == nil || t.backend == nil {
		return nil, fmt.Errorf("session_unavailable")
	}
	target, ok, err := t.state.LoadGatewayWorkspaceTarget(codespaceUUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("session_unavailable")
	}
	return t.backend.OpenWorkspaceCommand(ctx, provisioner.WorkspaceCommandRequest{
		InstanceName: target.instanceName,
		Workdir:      target.workdir,
		Interactive:  true,
		Cols:         120,
		Rows:         40,
	})
}

func (t *gatewayWorkspaceTerminal) copyOutput(conn *websocket.Conn, writerLock *sync.Mutex, reader io.Reader) {
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if writeErr := writeGatewayWebSSHBinaryLocked(conn, writerLock, buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func handleGatewayWebSSHControlInput(session provisioner.WorkspaceCommandSession, message []byte) error {
	var control gatewayWebSSHControlMessage
	if err := json.Unmarshal(message, &control); err != nil {
		return err
	}
	if control.Type != "resize" || control.Cols < 1 || control.Cols > 1000 || control.Rows < 1 || control.Rows > 1000 {
		return fmt.Errorf("invalid terminal control message")
	}
	return session.Resize(control.Cols, control.Rows)
}

func writeGatewayWebSSHControl(conn *websocket.Conn, message gatewayWebSSHControlMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(gatewayWebSSHWriteTime)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func writeGatewayWebSSHControlLocked(conn *websocket.Conn, writerLock *sync.Mutex, message gatewayWebSSHControlMessage) error {
	writerLock.Lock()
	defer writerLock.Unlock()
	return writeGatewayWebSSHControl(conn, message)
}

func writeGatewayWebSSHBinaryLocked(conn *websocket.Conn, writerLock *sync.Mutex, payload []byte) error {
	writerLock.Lock()
	defer writerLock.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(gatewayWebSSHWriteTime)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, payload)
}

func gatewayWebSSHExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *provisioner.WorkspaceCommandExitError
	if errors.As(err, &exitErr) {
		return exitErr.Status
	}
	return 255
}

func gatewayWebSSHErrorCategory(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "session_unavailable"):
		return "session_unavailable"
	case strings.Contains(text, "connect"), strings.Contains(text, "runtime"), strings.Contains(text, "incus"):
		return "runtime_unavailable"
	default:
		return "internal_error"
	}
}

const gatewayWebSSHPage = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Workspace</title>
<link rel="stylesheet" href="%s/terminal.css">
</head>
<body>
<main id="terminal" tabindex="0" aria-label="Workspace terminal"></main>
<script src="%s/terminal.js"></script>
</body>
</html>
`

const gatewayWebSSHScript = `"use strict";
const terminal = document.getElementById("terminal");
const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
const terminalPath = window.location.pathname === "/w/" ? "/.gitea-codespace/terminal" : new URL("./.gitea-codespace/terminal", window.location.href).pathname;
const socket = new WebSocket(scheme + "//" + window.location.host + terminalPath);
socket.binaryType = "arraybuffer";
function append(text) {
  terminal.textContent += text;
  terminal.scrollTop = terminal.scrollHeight;
}
socket.addEventListener("message", (event) => {
  if (typeof event.data === "string") {
    const message = JSON.parse(event.data);
    if (message.type === "ready") append("\r\n");
    if (message.type === "exit") append("\r\n[exit " + message.code + "]\r\n");
    if (message.type === "error") append("\r\n[" + message.category + "]\r\n");
    return;
  }
  append(new TextDecoder().decode(event.data));
});
terminal.addEventListener("keydown", (event) => {
  if (socket.readyState !== WebSocket.OPEN) return;
  if (event.key.length === 1) socket.send(new TextEncoder().encode(event.key));
  else if (event.key === "Enter") socket.send(new Uint8Array([13]));
  else if (event.key === "Backspace") socket.send(new Uint8Array([127]));
  event.preventDefault();
});
window.addEventListener("resize", () => {
  if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({type: "resize", cols: 120, rows: 40}));
});
terminal.focus();
`

const gatewayWebSSHStyle = `html, body, #terminal {
  box-sizing: border-box;
  width: 100%;
  height: 100%;
  margin: 0;
}
body {
  background: #101418;
  color: #d7dde5;
}
#terminal {
  overflow: auto;
  padding: 12px;
  white-space: pre-wrap;
  word-break: break-word;
  outline: none;
  font: 14px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
`
