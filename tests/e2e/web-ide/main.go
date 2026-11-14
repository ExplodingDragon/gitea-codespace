// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Command web-ide-e2e implements the provider lifecycle used by the platform
// E2E suite. It is deliberately small so tests exercise the provider contract
// without depending on a production Web IDE release.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

type prepareRequest struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	Initialize        bool           `json:"initialize"`
	TemplateOptions   map[string]any `json:"templateOptions"`
	RepositoryOptions map[string]any `json:"repositoryOptions"`
}

type providerState struct {
	PrepareCount    int            `json:"prepareCount"`
	InitializeCount int            `json:"initializeCount"`
	ServeCount      int            `json:"serveCount"`
	LastRequest     prepareRequest `json:"lastRequest"`
}

func main() {
	if len(os.Args) != 2 {
		fatalf("usage: web-ide-e2e prepare|serve")
	}
	switch os.Args[1] {
	case "prepare":
		prepare()
	case "serve":
		serve()
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func prepare() {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(requiredEnvironment("CODESPACE_WEB_IDE_PROGRAM_DIR"), &filesystem); err != nil {
		fatalf("inspect provider program mount: %v", err)
	}
	if filesystem.Flags&unix.ST_RDONLY == 0 {
		fatalf("provider program mount must be read-only")
	}
	data := requiredEnvironment("CODESPACE_WEB_IDE_DATA_DIR")
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024+1))
	if err != nil || len(input) > 64*1024 {
		fatalf("read prepare request: %v", err)
	}
	var request prepareRequest
	if err := json.Unmarshal(input, &request); err != nil || request.ProtocolVersion != 1 {
		fatalf("decode prepare request: %v", err)
	}
	state := readState(data)
	state.PrepareCount++
	if request.Initialize {
		state.InitializeCount++
	}
	state.LastRequest = request
	writeState(data, state)
}

func serve() {
	data := requiredEnvironment("CODESPACE_WEB_IDE_DATA_DIR")
	address := requiredEnvironment("CODESPACE_WEB_IDE_ADDRESS")
	state := readState(data)
	state.ServeCount++
	writeState(data, state)

	var stateLock sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Gitea Codespace fake Web IDE\n")
	})
	mux.HandleFunc("/__e2e/state", func(w http.ResponseWriter, _ *http.Request) {
		stateLock.Lock()
		defer stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(readState(data))
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/__e2e/ws", func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		kind, message, err := connection.ReadMessage()
		if err == nil {
			_ = connection.WriteMessage(kind, message)
		}
	})
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		fatalf("serve fake Web IDE: %v", err)
	}
}

func readState(directory string) providerState {
	data, err := os.ReadFile(filepath.Join(directory, "e2e-state.json"))
	if os.IsNotExist(err) {
		return providerState{}
	}
	if err != nil {
		fatalf("read provider state: %v", err)
	}
	var state providerState
	if err := json.Unmarshal(data, &state); err != nil {
		fatalf("decode provider state: %v", err)
	}
	return state
}

func writeState(directory string, state providerState) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		fatalf("create provider data directory: %v", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		fatalf("encode provider state: %v", err)
	}
	temporary := filepath.Join(directory, ".e2e-state.tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		fatalf("write provider state: %v", err)
	}
	if err := os.Rename(temporary, filepath.Join(directory, "e2e-state.json")); err != nil {
		fatalf("publish provider state: %v", err)
	}
}

func requiredEnvironment(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fatalf("%s is required", name)
	}
	return value
}

func fatalf(format string, values ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
