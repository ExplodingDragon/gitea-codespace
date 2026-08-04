// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

const (
	defaultWorkspaceCommandCols = 120
	defaultWorkspaceCommandRows = 40
	workspaceCommandControlWait = 2 * time.Second
	workspaceTCPDialTimeout     = 10 * time.Second
	workspaceIDEReadyTimeout    = 30 * time.Second
	workspaceIDELogTailBytes    = 4 * 1024
)

// OpenWorkspaceCommand opens one Gateway user shell or exec command through Incus exec.
func (p *IncusProvisioner) OpenWorkspaceCommand(ctx context.Context, request WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.InstanceName) == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if err := p.waitInstanceFileAPI(ctx, request.InstanceName); err != nil {
		return nil, err
	}
	cols, rows := normalizeWorkspaceTerminalSize(request.Cols, request.Rows)
	command := []string{runtimeExecutable, "runtime", "exec", "--state", runtimeEnvironmentFile, "--secrets", runtimeSecretFile}
	if request.Interactive {
		command = append(command, "--interactive")
	}
	command = append(command, "--", "/bin/sh")
	if request.Command != "" {
		command = append(command, "-lc", request.Command)
	} else {
		command = append(command, "-l")
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	session := &incusWorkspaceCommandSession{
		stdin:        stdinWriter,
		stdout:       stdoutReader,
		stderr:       stderrReader,
		stdoutClose:  stdoutWriter,
		stderrClose:  stderrWriter,
		cancel:       cancel,
		controlReady: make(chan struct{}),
		dataDone:     make(chan bool),
		waitDone:     make(chan error, 1),
	}
	args := &incus.InstanceExecArgs{
		Stdin:    stdinReader,
		Stdout:   stdoutWriter,
		Stderr:   stderrWriter,
		Control:  session.setControl,
		DataDone: session.dataDone,
	}
	operation, err := p.client.ExecInstance(request.InstanceName, api.InstanceExecPost{
		Command:     command,
		WaitForWS:   true,
		Interactive: request.Interactive,
		Environment: nil,
		Width:       cols,
		Height:      rows,
		User:        0,
		Group:       0,
		Cwd:         "/",
	}, args)
	if err != nil {
		cancel()
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("exec workspace command: %w", err)
	}
	session.operation = operation
	go session.wait(runCtx)
	return session, nil
}

type incusWorkspaceCommandSession struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	stderr *io.PipeReader

	stdoutClose *io.PipeWriter
	stderrClose *io.PipeWriter
	cancel      context.CancelFunc
	operation   incus.Operation
	dataDone    chan bool
	waitDone    chan error

	controlMu        sync.Mutex
	control          *websocket.Conn
	controlReady     chan struct{}
	controlReadyOnce sync.Once
	closeOnce        sync.Once
}

func (s *incusWorkspaceCommandSession) Stdin() io.WriteCloser {
	return s.stdin
}

func (s *incusWorkspaceCommandSession) Stdout() io.Reader {
	return s.stdout
}

func (s *incusWorkspaceCommandSession) Stderr() io.Reader {
	return s.stderr
}

func (s *incusWorkspaceCommandSession) Resize(cols, rows int) error {
	cols, rows = normalizeWorkspaceTerminalSize(cols, rows)
	return s.writeControl(api.InstanceExecControl{
		Command: "window-resize",
		Args: map[string]string{
			"width":  strconv.Itoa(cols),
			"height": strconv.Itoa(rows),
		},
	})
}

func (s *incusWorkspaceCommandSession) Signal(signal int) error {
	if signal <= 0 {
		return fmt.Errorf("workspace command signal must be positive")
	}
	return s.writeControl(api.InstanceExecControl{Command: "signal", Signal: signal})
}

func (s *incusWorkspaceCommandSession) writeControl(message api.InstanceExecControl) error {
	if err := s.waitControlReady(); err != nil {
		return err
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	control := s.control
	if control == nil {
		return fmt.Errorf("workspace command control socket is unavailable")
	}
	return control.WriteJSON(message)
}

func (s *incusWorkspaceCommandSession) Wait() error {
	return <-s.waitDone
}

func (s *incusWorkspaceCommandSession) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.controlReadyOnce.Do(func() {
			close(s.controlReady)
		})
		_ = s.stdin.Close()
		if s.operation != nil {
			_ = s.operation.Cancel()
		}
	})
	return nil
}

func (s *incusWorkspaceCommandSession) setControl(control *websocket.Conn) {
	s.controlMu.Lock()
	s.control = control
	s.controlMu.Unlock()
	s.controlReadyOnce.Do(func() {
		close(s.controlReady)
	})
}

func (s *incusWorkspaceCommandSession) waitControlReady() error {
	select {
	case <-s.controlReady:
		return nil
	case <-time.After(workspaceCommandControlWait):
		return fmt.Errorf("workspace command control socket is unavailable")
	}
}

func (s *incusWorkspaceCommandSession) wait(ctx context.Context) {
	err := s.operation.WaitContext(ctx)
	if s.dataDone != nil {
		select {
		case <-s.dataDone:
		case <-ctx.Done():
		}
	}
	_ = s.stdoutClose.Close()
	_ = s.stderrClose.Close()
	if err == nil {
		if status, ok := incusOperationExitStatus(s.operation); ok && status != 0 {
			err = &WorkspaceCommandExitError{Status: status}
		}
	}
	s.waitDone <- err
}

func incusOperationExitStatus(operation incus.Operation) (int, bool) {
	if operation == nil {
		return 0, false
	}
	raw, ok := operation.Get().Metadata["return"]
	if !ok {
		return 0, false
	}
	switch value := raw.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}

func normalizeWorkspaceTerminalSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = defaultWorkspaceCommandCols
	}
	if rows <= 0 {
		rows = defaultWorkspaceCommandRows
	}
	return cols, rows
}

// OpenWorkspaceTCP connects one port on the current runtime instance.
func (p *IncusProvisioner) OpenWorkspaceTCP(ctx context.Context, instanceName string, port uint32) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instanceName = strings.TrimSpace(instanceName)
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if port == 0 || port > 65535 {
		return nil, fmt.Errorf("workspace tcp port is invalid")
	}
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return nil, err
	}
	clientConn, runtimeConn := net.Pipe()
	dataDone := make(chan bool)
	operation, err := p.client.ExecInstance(instanceName, api.InstanceExecPost{
		Command: []string{
			runtimeExecutable, "runtime", "tcp",
			"--state", runtimeEnvironmentFile,
			"--port", strconv.Itoa(int(port)),
		},
		WaitForWS: true,
		User:      0,
		Group:     0,
		Cwd:       "/",
	}, &incus.InstanceExecArgs{
		Stdin:    runtimeConn,
		Stdout:   runtimeConn,
		Stderr:   io.Discard,
		DataDone: dataDone,
	})
	if err != nil {
		_ = clientConn.Close()
		_ = runtimeConn.Close()
		return nil, fmt.Errorf("open Dev Container localhost port %d: %w", port, err)
	}
	connection := &incusWorkspaceTCPConn{Conn: clientConn, operation: operation}
	go func() {
		_ = operation.WaitContext(ctx)
		select {
		case <-dataDone:
		case <-ctx.Done():
		}
		_ = runtimeConn.Close()
		_ = clientConn.Close()
	}()
	return connection, nil
}

type incusWorkspaceTCPConn struct {
	net.Conn
	operation incus.Operation
	once      sync.Once
}

func (connection *incusWorkspaceTCPConn) Close() error {
	var err error
	connection.once.Do(func() {
		if connection.operation != nil {
			_ = connection.operation.Cancel()
		}
		err = connection.Conn.Close()
	})
	return err
}

// CheckWorkspaceIDE verifies that code-server is serving its health endpoint.
func (p *IncusProvisioner) CheckWorkspaceIDE(ctx context.Context, instanceName string, port uint32) error {
	readyDeadline := time.Now().Add(workspaceIDEReadyTimeout)
	var lastErr error
	for {
		if err := p.checkWorkspaceIDEOnce(ctx, instanceName, port); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(readyDeadline) {
			logContent, exists, err := p.readRuntimeFile(ctx, instanceName, runtimeCodeServerLogFile)
			if err != nil || !exists || strings.TrimSpace(logContent) == "" {
				return fmt.Errorf("wait for code-server readiness: %w", lastErr)
			}
			if len(logContent) > workspaceIDELogTailBytes {
				logContent = logContent[len(logContent)-workspaceIDELogTailBytes:]
			}
			return fmt.Errorf("wait for code-server readiness: %w; recent code-server log:\n%s", lastErr, strings.TrimSpace(logContent))
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *IncusProvisioner) checkWorkspaceIDEOnce(ctx context.Context, instanceName string, port uint32) error {
	conn, err := p.OpenWorkspaceTCP(ctx, instanceName, port)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(workspaceTCPDialTimeout))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://codespace-workspace/healthz", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Connection", "close")
	if err := request.Write(conn); err != nil {
		return fmt.Errorf("request code-server health: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return fmt.Errorf("read code-server health: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("code-server health returned %s", response.Status)
	}
	return nil
}

// OpenWorkspaceSFTP opens an instance SFTP subsystem at the workspace directory.
func (p *IncusProvisioner) OpenWorkspaceSFTP(ctx context.Context, request WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.InstanceName) == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	workdir := strings.TrimSpace(request.Workdir)
	if !path.IsAbs(workdir) {
		return nil, fmt.Errorf("workdir must be absolute")
	}
	if request.User == 0 || request.Group == 0 {
		return nil, fmt.Errorf("workspace user and group are required")
	}
	if err := p.waitInstanceFileAPI(ctx, request.InstanceName); err != nil {
		return nil, err
	}
	client, err := p.client.GetInstanceFileSFTP(request.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("open instance sftp: %w", err)
	}
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, workspaceSFTPHandlers(client, workdir, request.User, request.Group), sftp.WithStartDirectory(path.Clean(workdir)))
	go func() {
		_ = server.Serve()
		_ = serverConn.Close()
		_ = client.Close()
	}()
	return clientConn, nil
}

func workspaceSFTPHandlers(client *sftp.Client, workdir string, uid, gid uint32) sftp.Handlers {
	handler := &workspaceSFTPHandler{
		client:  client,
		workdir: path.Clean(workdir),
		uid:     uid,
		gid:     gid,
	}
	return sftp.Handlers{
		FileGet:  handler,
		FilePut:  handler,
		FileCmd:  handler,
		FileList: handler,
	}
}

type workspaceSFTPHandler struct {
	client  *sftp.Client
	workdir string
	uid     uint32
	gid     uint32
}

func (h *workspaceSFTPHandler) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	return h.client.OpenFile(h.path(request.Filepath), os.O_RDONLY)
}

func (h *workspaceSFTPHandler) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	return h.openFile(request)
}

func (h *workspaceSFTPHandler) OpenFile(request *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.openFile(request)
}

func (h *workspaceSFTPHandler) Filecmd(request *sftp.Request) error {
	switch request.Method {
	case "Setstat":
		return h.setStat(request)
	case "Rename", "PosixRename":
		return h.client.Rename(h.path(request.Filepath), h.path(request.Target))
	case "Rmdir":
		return h.client.RemoveDirectory(h.path(request.Filepath))
	case "Remove":
		return h.client.Remove(h.path(request.Filepath))
	case "Mkdir":
		path := h.path(request.Filepath)
		if err := h.client.Mkdir(path); err != nil {
			return err
		}
		return h.chown(path)
	case "Link":
		return h.client.Link(h.path(request.Filepath), h.path(request.Target))
	case "Symlink":
		return h.client.Symlink(request.Filepath, h.path(request.Target))
	default:
		return fmt.Errorf("sftp command %s is unsupported", request.Method)
	}
}

func (h *workspaceSFTPHandler) PosixRename(request *sftp.Request) error {
	return h.client.PosixRename(h.path(request.Filepath), h.path(request.Target))
}

func (h *workspaceSFTPHandler) StatVFS(request *sftp.Request) (*sftp.StatVFS, error) {
	return h.client.StatVFS(h.path(request.Filepath))
}

func (h *workspaceSFTPHandler) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	switch request.Method {
	case "List":
		entries, err := h.client.ReadDir(h.path(request.Filepath))
		if err != nil {
			return nil, err
		}
		return workspaceSFTPList(entries), nil
	case "Stat":
		info, err := h.client.Stat(h.path(request.Filepath))
		if err != nil {
			return nil, err
		}
		return workspaceSFTPList{info}, nil
	default:
		return nil, fmt.Errorf("sftp list method %s is unsupported", request.Method)
	}
}

func (h *workspaceSFTPHandler) Lstat(request *sftp.Request) (sftp.ListerAt, error) {
	info, err := h.client.Lstat(h.path(request.Filepath))
	if err != nil {
		return nil, err
	}
	return workspaceSFTPList{info}, nil
}

func (h *workspaceSFTPHandler) RealPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return h.workdir, nil
	}
	if path.IsAbs(value) {
		return path.Clean(value), nil
	}
	return path.Clean(path.Join(h.workdir, value)), nil
}

func (h *workspaceSFTPHandler) Readlink(value string) (string, error) {
	return h.client.ReadLink(h.path(value))
}

func (h *workspaceSFTPHandler) openFile(request *sftp.Request) (*sftp.File, error) {
	path := h.path(request.Filepath)
	_, statErr := h.client.Stat(path)
	existed := statErr == nil
	file, err := h.client.OpenFile(path, workspaceSFTPOpenFlags(request.Pflags()))
	if err != nil {
		return nil, err
	}
	if request.Pflags().Creat && !existed {
		if err := h.chown(path); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func (h *workspaceSFTPHandler) chown(path string) error {
	if h.uid == 0 || h.gid == 0 {
		return nil
	}
	return h.client.Chown(path, int(h.uid), int(h.gid))
}

func (h *workspaceSFTPHandler) setStat(request *sftp.Request) error {
	path := h.path(request.Filepath)
	flags := request.AttrFlags()
	attributes := request.Attributes()
	if flags.Size {
		if err := h.client.Truncate(path, int64(attributes.Size)); err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := h.client.Chmod(path, attributes.FileMode()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.client.Chtimes(path, attributes.AccessTime(), attributes.ModTime()); err != nil {
			return err
		}
	}
	if flags.UidGid {
		return h.client.Chown(path, int(attributes.UID), int(attributes.GID))
	}
	return nil
}

func (h *workspaceSFTPHandler) path(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(value, "/"))
}

func workspaceSFTPOpenFlags(flags sftp.FileOpenFlags) int {
	result := 0
	switch {
	case flags.Read && flags.Write:
		result |= os.O_RDWR
	case flags.Write:
		result |= os.O_WRONLY
	default:
		result |= os.O_RDONLY
	}
	if flags.Append {
		result |= os.O_APPEND
	}
	if flags.Creat {
		result |= os.O_CREATE
	}
	if flags.Trunc {
		result |= os.O_TRUNC
	}
	if flags.Excl {
		result |= os.O_EXCL
	}
	return result
}

type workspaceSFTPList []os.FileInfo

func (l workspaceSFTPList) ListAt(entries []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(entries, l[offset:])
	if n < len(entries) {
		return n, io.EOF
	}
	return n, nil
}
