// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"
)

const (
	defaultWorkspaceCommandCols = 120
	defaultWorkspaceCommandRows = 40
)

// OpenWorkspaceCommand opens one Gateway user shell or exec command through Incus exec.
func (p *IncusProvisioner) OpenWorkspaceCommand(ctx context.Context, request WorkspaceCommandRequest) (WorkspaceCommandSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.InstanceName) == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if strings.TrimSpace(request.Workdir) == "" {
		return nil, fmt.Errorf("workdir is empty")
	}
	if err := p.waitInstanceFileAPI(ctx, request.InstanceName); err != nil {
		return nil, err
	}
	cols, rows := normalizeWorkspaceTerminalSize(request.Cols, request.Rows)
	command := []string{p.bootstrap.Shell}
	if request.Command != "" {
		command = []string{p.bootstrap.Shell, "-lc", request.Command}
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	session := &incusWorkspaceCommandSession{
		stdin:       stdinWriter,
		stdout:      stdoutReader,
		stderr:      stderrReader,
		stdoutClose: stdoutWriter,
		stderrClose: stderrWriter,
		cancel:      cancel,
		dataDone:    make(chan bool),
		waitDone:    make(chan error, 1),
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
		Width:       cols,
		Height:      rows,
		User:        p.bootstrap.User,
		Group:       p.bootstrap.Group,
		Cwd:         request.Workdir,
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

	controlMu sync.Mutex
	control   *websocket.Conn
	closeOnce sync.Once
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
	s.controlMu.Lock()
	control := s.control
	s.controlMu.Unlock()
	if control == nil {
		return nil
	}
	return control.WriteJSON(api.InstanceExecControl{
		Command: "window-resize",
		Args: map[string]string{
			"width":  strconv.Itoa(cols),
			"height": strconv.Itoa(rows),
		},
	})
}

func (s *incusWorkspaceCommandSession) Wait() error {
	return <-s.waitDone
}

func (s *incusWorkspaceCommandSession) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
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
	_, _, _ = control.ReadMessage()
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

// OpenWorkspaceSFTP opens one workspace-rooted SFTP subsystem through Incus file APIs.
func (p *IncusProvisioner) OpenWorkspaceSFTP(ctx context.Context, request WorkspaceSFTPRequest) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.InstanceName) == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	if strings.TrimSpace(request.Workdir) == "" {
		return nil, fmt.Errorf("workdir is empty")
	}
	if err := p.waitInstanceFileAPI(ctx, request.InstanceName); err != nil {
		return nil, err
	}
	client, err := p.client.GetInstanceFileSFTP(request.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("open instance sftp: %w", err)
	}
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, workspaceSFTPHandlers(client, request.Workdir), sftp.WithStartDirectory("/"))
	go func() {
		_ = server.Serve()
		_ = serverConn.Close()
		_ = client.Close()
	}()
	return clientConn, nil
}

func workspaceSFTPHandlers(client *sftp.Client, workdir string) sftp.Handlers {
	handler := &workspaceSFTPHandler{
		client:  client,
		workdir: path.Clean("/" + strings.Trim(strings.TrimSpace(workdir), "/")),
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
		return h.client.Mkdir(h.path(request.Filepath))
	default:
		return fmt.Errorf("sftp command %s is unsupported", request.Method)
	}
}

func (h *workspaceSFTPHandler) PosixRename(request *sftp.Request) error {
	return h.client.PosixRename(h.path(request.Filepath), h.path(request.Target))
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
	cleaned := workspaceSFTPClientPath(value)
	return cleaned, nil
}

func (h *workspaceSFTPHandler) Readlink(value string) (string, error) {
	target, err := h.client.ReadLink(h.path(value))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(target, h.workdir+"/") || target == h.workdir {
		relative := strings.TrimPrefix(strings.TrimPrefix(target, h.workdir), "/")
		if relative == "" {
			return "/", nil
		}
		return "/" + relative, nil
	}
	return target, nil
}

func (h *workspaceSFTPHandler) openFile(request *sftp.Request) (*sftp.File, error) {
	return h.client.OpenFile(h.path(request.Filepath), workspaceSFTPOpenFlags(request.Pflags()))
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
		return fmt.Errorf("sftp uid/gid change is unsupported")
	}
	return nil
}

func (h *workspaceSFTPHandler) path(value string) string {
	cleaned := workspaceSFTPClientPath(value)
	if cleaned == "/" {
		return h.workdir
	}
	return path.Join(h.workdir, strings.TrimPrefix(cleaned, "/"))
}

func workspaceSFTPClientPath(value string) string {
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
