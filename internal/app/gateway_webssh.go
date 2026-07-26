// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	_ "embed"
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

//go:embed webssh_assets/xterm.js
var gatewayWebSSHXtermScript string

//go:embed webssh_assets/xterm.css
var gatewayWebSSHXtermStyle string

//go:embed webssh_assets/xterm-addon-fit.js
var gatewayWebSSHXtermFitScript string

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
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Workspace terminal is starting", "Codespace Gateway cannot open the built-in workspace terminal yet. Try again shortly.", "gateway web ssh is not ready")
		return
	}
	if request.Method != http.MethodGet {
		writeGatewayError(writer, request, http.StatusMethodNotAllowed, "Method not allowed", "The built-in workspace terminal only accepts GET requests.", "method_not_allowed")
		return
	}
	switch upstreamPath {
	case "/":
		serveGatewayWebSSHPage(writer, request)
	case "/.gitea-codespace/assets/xterm.js":
		serveGatewayWebSSHXtermScript(writer)
	case "/.gitea-codespace/assets/xterm.css":
		serveGatewayWebSSHXtermStyle(writer)
	case "/.gitea-codespace/assets/xterm-addon-fit.js":
		serveGatewayWebSSHXtermFitScript(writer)
	case "/.gitea-codespace/assets/terminal.js":
		serveGatewayWebSSHScript(writer)
	case "/.gitea-codespace/assets/terminal.css":
		serveGatewayWebSSHStyle(writer)
	case "/.gitea-codespace/terminal":
		t.serveTerminal(writer, request, codespaceUUID)
	default:
		writeGatewayNotFound(writer, request, "Workspace terminal")
	}
}

func serveGatewayWebSSHPage(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	assetBase := "./.gitea-codespace/assets"
	if request != nil && request.URL.Path == "/w/" {
		assetBase = "/.gitea-codespace/assets"
	}
	_, _ = fmt.Fprintf(writer, gatewayWebSSHPage, assetBase, assetBase, assetBase, assetBase, assetBase)
}

func serveGatewayWebSSHScript(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHScript)
}

func serveGatewayWebSSHXtermScript(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHXtermScript)
}

func serveGatewayWebSSHXtermStyle(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/css; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHXtermStyle)
}

func serveGatewayWebSSHXtermFitScript(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, gatewayWebSSHXtermFitScript)
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
		User:         target.uid,
		Group:        target.gid,
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
<link rel="stylesheet" href="%s/xterm.css">
<link rel="stylesheet" href="%s/terminal.css">
</head>
<body>
<main class="workspace-shell">
<header>
<div>
<div class="eyebrow">Codespace</div>
<h1>Workspace terminal</h1>
</div>
<div class="status" id="status"><span></span>Connecting</div>
</header>
<section id="terminal" tabindex="0" aria-label="Workspace terminal"></section>
</main>
<script src="%s/xterm.js"></script>
<script src="%s/xterm-addon-fit.js"></script>
<script src="%s/terminal.js"></script>
</body>
</html>
`

const gatewayWebSSHScript = `"use strict";
const terminal = document.getElementById("terminal");
const status = document.getElementById("status");
const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
const terminalPath = window.location.pathname === "/w/" ? "/.gitea-codespace/terminal" : new URL("./.gitea-codespace/terminal", window.location.href).pathname;
const socket = new WebSocket(scheme + "//" + window.location.host + terminalPath);
socket.binaryType = "arraybuffer";
const decoder = new TextDecoder();
const encoder = new TextEncoder();
const term = new Terminal({
  cols: 120,
  rows: 40,
  cursorBlink: true,
  convertEol: false,
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
  fontSize: 14,
  theme: {
    background: "#101418",
    foreground: "#d7dde5",
    cursor: "#d7dde5"
  }
});
const fitAddon = new FitAddon.FitAddon();
term.loadAddon(fitAddon);
term.open(terminal);
term.focus();
let resizeFrame = 0;
let pendingFitTimer = 0;
let pendingFitAttempts = 0;
let lastSentCols = 0;
let lastSentRows = 0;
function setStatus(text, className) {
  status.lastChild.textContent = text;
  status.className = "status " + className;
}
function sendTerminalSize(cols, rows) {
  if (socket.readyState !== WebSocket.OPEN) return;
  if (cols === lastSentCols && rows === lastSentRows) return;
  lastSentCols = cols;
  lastSentRows = rows;
  socket.send(JSON.stringify({type: "resize", cols: cols, rows: rows}));
}
function fitTerminal() {
  if (terminal.clientWidth <= 0 || terminal.clientHeight <= 0) {
    retryTerminalFit();
    return;
  }
  const dimensions = fitAddon.proposeDimensions();
  if (!dimensions || dimensions.cols < 2 || dimensions.rows < 1) {
    retryTerminalFit();
    return;
  }
  fitAddon.fit();
  const cols = Math.max(1, Math.min(1000, term.cols));
  const rows = Math.max(1, Math.min(1000, term.rows));
  sendTerminalSize(cols, rows);
  pendingFitAttempts = 0;
}
function retryTerminalFit() {
  if (pendingFitTimer !== 0 || pendingFitAttempts >= 20) return;
  pendingFitAttempts++;
  pendingFitTimer = window.setTimeout(() => {
    pendingFitTimer = 0;
    scheduleTerminalFit();
  }, 50);
}
function scheduleTerminalFit() {
  if (resizeFrame !== 0) return;
  resizeFrame = requestAnimationFrame(() => {
    resizeFrame = 0;
    fitTerminal();
  });
}
term.onData((data) => {
  if (socket.readyState === WebSocket.OPEN) socket.send(encoder.encode(data));
});
term.onResize((size) => sendTerminalSize(size.cols, size.rows));
socket.addEventListener("open", () => {
  scheduleTerminalFit();
  setStatus("Connecting shell", "connecting");
});
socket.addEventListener("message", (event) => {
  if (typeof event.data === "string") {
    const message = JSON.parse(event.data);
    if (message.type === "ready") {
      setStatus("Connected", "connected");
      scheduleTerminalFit();
      term.write("\r\n");
    }
    if (message.type === "exit") {
      setStatus("Exited", "closed");
      term.write("\r\n[exit " + message.code + "]\r\n");
    }
    if (message.type === "error") {
      setStatus("Unavailable", "closed");
      term.write("\r\n[" + message.category + "]\r\n");
    }
    return;
  }
  term.write(decoder.decode(event.data));
  scheduleTerminalFit();
});
socket.addEventListener("close", () => setStatus("Disconnected", "closed"));
if ("ResizeObserver" in window) new ResizeObserver(scheduleTerminalFit).observe(terminal);
window.addEventListener("resize", scheduleTerminalFit);
window.addEventListener("load", scheduleTerminalFit);
if (document.fonts) document.fonts.ready.then(scheduleTerminalFit);
scheduleTerminalFit();
setTimeout(scheduleTerminalFit, 50);
`

const gatewayWebSSHStyle = `html, body {
  box-sizing: border-box;
  width: 100%;
  height: 100%;
  margin: 0;
}
html {
  overflow: hidden;
}
*, *::before, *::after {
  box-sizing: inherit;
}
body {
  overflow: hidden;
  background: #0d1117;
  color: #d7dee8;
  font: 14px/1.4 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
.workspace-shell {
  position: fixed;
  inset: 0;
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  width: 100%;
  height: 100%;
  height: 100vh;
  height: 100dvh;
  min-width: 0;
  min-height: 0;
  background: #0d1117;
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  min-height: 58px;
  padding: 10px 14px;
  border-bottom: 1px solid #242c36;
  background: #141a21;
}
.eyebrow {
  color: #8493a5;
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
}
h1 {
  margin: 1px 0 0;
  color: #eef3f8;
  font-size: 15px;
  font-weight: 600;
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-width: max-content;
  color: #9aa9b8;
  font-size: 12px;
}
.status span {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #d99a2b;
}
.status.connected span {
  background: #2fbd6a;
}
.status.closed span {
  background: #d65f5f;
}
#terminal {
  position: relative;
  display: block;
  width: 100%;
  height: 100%;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
  background: #101418;
  outline: none;
}
#terminal .xterm {
  width: 100%;
  height: 100%;
  background: #101418;
}
#terminal .xterm-viewport {
  background-color: transparent;
}
`
