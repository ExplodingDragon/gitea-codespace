// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"gitea.dev/codespace/internal/provisioner"
)

const gatewaySSHUserPrefix = "cs-"
const (
	defaultGatewaySSHCols = 120
	defaultGatewaySSHRows = 40
)

type gatewaySSHPty struct {
	enabled bool
	cols    int
	rows    int
}

type gatewayWorkspaceTargetStore interface {
	LoadGatewayWorkspaceTarget(codespaceUUID string) (gatewayWorkspaceTarget, bool, error)
}

type gatewayWorkspaceCommandBackend interface {
	OpenWorkspaceCommand(ctx context.Context, request provisioner.WorkspaceCommandRequest) (provisioner.WorkspaceCommandSession, error)
}

type gatewayWorkspaceBackend interface {
	gatewayWorkspaceCommandBackend
	OpenWorkspaceSFTP(ctx context.Context, request provisioner.WorkspaceSFTPRequest) (io.ReadWriteCloser, error)
	OpenWorkspaceTCP(ctx context.Context, instanceName string, port uint32) (net.Conn, error)
	CheckWorkspaceAccess(ctx context.Context, instanceName, workdir string) error
	CheckDevContainer(ctx context.Context, instanceName string) error
}

type gatewaySSHServer struct {
	config             *ssh.ServerConfig
	state              gatewayWorkspaceTargetStore
	backend            gatewayWorkspaceBackend
	controlPlane       *gatewayControlPlane
	sessions           *gatewaySessionRegistry
	access             *gatewayAccessController
	authLimiter        *gatewaySSHAuthLimiter
	handshakeTimeout   time.Duration
	sessionIdleTimeout time.Duration
	revalidateInterval time.Duration
	maxChannels        int
}

type gatewaySSHAuthContext struct {
	codespaceUUID string
	userID        int64
}

func newGatewaySSHServer(
	hostKey ssh.Signer,
	state gatewayWorkspaceTargetStore,
	backend gatewayWorkspaceBackend,
	controlPlane *gatewayControlPlane,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	gatewayConfig GatewayConfig,
) (*gatewaySSHServer, error) {
	if hostKey == nil {
		return nil, fmt.Errorf("gateway ssh host key is required")
	}
	idleTimeout := gatewayConfig.Sessions.IdleTimeout.ToStdlib()
	if idleTimeout <= 0 {
		idleTimeout = DefaultConfig().Gateway.Sessions.IdleTimeout.ToStdlib()
	}
	revalidateInterval := gatewayConfig.Sessions.RevalidateInterval.ToStdlib()
	if revalidateInterval <= 0 {
		revalidateInterval = defaultGatewaySessionRevalidateInterval
	}
	maxChannels := gatewayConfig.SSH.MaxChannelsPerConnection
	if maxChannels <= 0 {
		maxChannels = DefaultConfig().Gateway.SSH.MaxChannelsPerConnection
	}
	handshakeTimeout := gatewayConfig.SSH.HandshakeTimeout.ToStdlib()
	if handshakeTimeout <= 0 {
		handshakeTimeout = DefaultConfig().Gateway.SSH.HandshakeTimeout.ToStdlib()
	}
	server := &gatewaySSHServer{
		state:              state,
		backend:            backend,
		controlPlane:       controlPlane,
		sessions:           sessions,
		access:             access,
		authLimiter:        newGatewaySSHAuthLimiterFromConfig(gatewayConfig),
		handshakeTimeout:   handshakeTimeout,
		sessionIdleTimeout: idleTimeout,
		revalidateInterval: revalidateInterval,
		maxChannels:        maxChannels,
	}
	config := &ssh.ServerConfig{ServerVersion: "SSH-2.0-gitea-codespace"}
	config.AddHostKey(hostKey)
	server.config = config
	return server, nil
}

func (s *gatewaySSHServer) authenticatePublicKey(ctx context.Context, conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	sourceIP := gatewaySSHSourceIP(conn.RemoteAddr())
	publicKeyHash := gatewaySSHPublicKeyHash(key)
	codespaceUUID, ok := codespaceUUIDFromGatewaySSHUser(conn.User())
	if !ok {
		s.authLimiter.RecordFailure(sourceIP, "", publicKeyHash, "invalid_credentials", time.Now())
		return nil, fmt.Errorf("invalid codespace ssh user")
	}
	if !s.authLimiter.Allow(sourceIP, codespaceUUID, publicKeyHash, time.Now()) {
		return nil, gatewaySSHAuthLimitError()
	}
	if s.controlPlane == nil {
		return nil, fmt.Errorf("gateway control plane is not ready")
	}
	decision, err := s.controlPlane.verifySSHPublicKey(ctx, codespaceUUID, key.Marshal())
	if err != nil {
		return nil, err
	}
	if !decision.allowed {
		s.authLimiter.RecordFailure(sourceIP, codespaceUUID, publicKeyHash, decision.deniedCategory, time.Now())
		return nil, fmt.Errorf("ssh public key denied: %s", decision.deniedCategory)
	}
	target, ok, err := s.loadWorkspaceTarget(codespaceUUID)
	if err != nil || !ok {
		return nil, fmt.Errorf("codespace workspace is unavailable")
	}
	if err := s.backend.CheckWorkspaceAccess(ctx, target.instanceName, target.workdir); err != nil {
		return nil, fmt.Errorf("codespace workspace is unavailable")
	}
	if err := s.backend.CheckDevContainer(ctx, target.instanceName); err != nil {
		return nil, fmt.Errorf("codespace Dev Container is unavailable")
	}
	return &ssh.Permissions{
		Extensions: map[string]string{
			"codespace_uuid": codespaceUUID,
			"user_id":        formatInt64(decision.userID),
		},
	}, nil
}

func (s *gatewaySSHServer) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	releaseTransport, ok := s.reserveTransport()
	if !ok {
		return
	}
	defer releaseTransport()
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, s.handshakeTimeout)
	handshakeDone := make(chan struct{})
	handshakeWatcherDone := make(chan struct{})
	go func() {
		defer close(handshakeWatcherDone)
		select {
		case <-handshakeCtx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(s.handshakeTimeout))
	config := *s.config
	config.PublicKeyCallback = func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		return s.authenticatePublicKey(handshakeCtx, metadata, key)
	}
	sshConn, channels, requests, err := ssh.NewServerConn(conn, &config)
	close(handshakeDone)
	<-handshakeWatcherDone
	cancelHandshake()
	if err != nil {
		log.Printf("gateway ssh handshake: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(requests)

	auth, ok := gatewaySSHAuthFromPermissions(sshConn.Permissions)
	if !ok {
		return
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeGatewaySSHConnOnDone(sessionCtx, sshConn)
	release := func() {}
	if s.sessions != nil {
		var ok bool
		release, ok = s.sessions.BeginSSHSession(auth.codespaceUUID, auth.userID, cancel, time.Now())
		if !ok {
			return
		}
	}
	defer release()
	if _, ok, err := s.loadWorkspaceTarget(auth.codespaceUUID); err != nil || !ok {
		return
	}
	activity := make(chan struct{}, 1)
	go s.revalidateSession(sessionCtx, auth, cancel)
	go s.cancelIdleSession(sessionCtx, cancel, activity)
	channelSlots := make(chan struct{}, s.maxChannels)

	for channel := range channels {
		select {
		case channelSlots <- struct{}{}:
		default:
			_ = channel.Reject(ssh.ResourceShortage, "too many ssh channels")
			continue
		}
		notifyGatewaySSHActivity(activity)
		go func() {
			defer func() {
				<-channelSlots
			}()
			s.handleChannel(sessionCtx, auth, channel, activity)
		}()
	}
}

func (s *gatewaySSHServer) reserveTransport() (func(), bool) {
	if s.access == nil {
		return func() {}, true
	}
	reservation, status := s.access.reserveRequest()
	if status != 0 {
		return nil, false
	}
	return reservation.Release, true
}

func closeGatewaySSHConnOnDone(ctx context.Context, conn ssh.Conn) {
	<-ctx.Done()
	_ = conn.Close()
}

func (s *gatewaySSHServer) cancelIdleSession(ctx context.Context, cancel context.CancelFunc, activity <-chan struct{}) {
	timer := time.NewTimer(s.sessionIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.sessionIdleTimeout)
		case <-timer.C:
			cancel()
			return
		}
	}
}

func (s *gatewaySSHServer) handleChannel(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
	if channel.ChannelType() == "direct-tcpip" {
		s.handleDirectTCPIP(ctx, auth, channel, activity)
		return
	}
	if channel.ChannelType() != "session" {
		_ = channel.Reject(ssh.UnknownChannelType, "unsupported channel type")
		return
	}
	clientChannel, requests, err := channel.Accept()
	if err != nil {
		return
	}
	defer func() { _ = clientChannel.Close() }()

	target, ok, err := s.loadWorkspaceTarget(auth.codespaceUUID)
	if err != nil || !ok {
		_ = clientChannel.Close()
		return
	}

	pty := gatewaySSHPty{cols: defaultGatewaySSHCols, rows: defaultGatewaySSHRows}
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		switch request.Type {
		case "pty-req":
			pty = parseGatewaySSHPty(request.Payload)
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
		case "window-change":
			pty = parseGatewaySSHWindowChange(request.Payload, pty)
		case "shell", "exec":
			command := ""
			if request.Type == "exec" {
				command = parseGatewaySSHExecCommand(request.Payload)
				if command == "" {
					if request.WantReply {
						_ = request.Reply(false, nil)
					}
					continue
				}
			}
			commandRequest := target.commandRequest()
			commandRequest.Command = command
			commandRequest.Interactive = pty.enabled
			commandRequest.Cols = pty.cols
			commandRequest.Rows = pty.rows
			session, err := s.backend.OpenWorkspaceCommand(ctx, commandRequest)
			if err != nil {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				return
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
			s.serveWorkspaceSession(ctx, clientChannel, requests, session, pty, activity)
			return
		case "subsystem":
			subsystem := parseGatewaySSHSubsystem(request.Payload)
			if subsystem != "sftp" {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				continue
			}
			conn, err := s.backend.OpenWorkspaceSFTP(ctx, provisioner.WorkspaceSFTPRequest{
				InstanceName: target.instanceName,
				Workdir:      target.workdir,
				User:         target.uid,
				Group:        target.gid,
			})
			if err != nil {
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				return
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
			s.proxyWorkspaceStream(ctx, clientChannel, requests, conn, activity)
			return
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func (s *gatewaySSHServer) loadWorkspaceTarget(codespaceUUID string) (gatewayWorkspaceTarget, bool, error) {
	if s.state == nil || s.backend == nil {
		return gatewayWorkspaceTarget{}, false, nil
	}
	return s.state.LoadGatewayWorkspaceTarget(codespaceUUID)
}

func (s *gatewaySSHServer) handleDirectTCPIP(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
	payload := struct {
		Host           string
		Port           uint32
		OriginatorHost string
		OriginatorPort uint32
	}{}
	if err := ssh.Unmarshal(channel.ExtraData(), &payload); err != nil ||
		strings.TrimSpace(payload.Host) == "" ||
		payload.Port == 0 ||
		payload.Port > 65535 {
		_ = channel.Reject(ssh.Prohibited, "invalid direct-tcpip target")
		return
	}
	targetHost := strings.TrimSpace(payload.Host)
	switch targetHost {
	case "localhost", "127.0.0.1", "::1":
	default:
		_ = channel.Reject(ssh.Prohibited, "direct-tcpip target must be localhost, 127.0.0.1, or ::1")
		return
	}
	target, ok, err := s.loadWorkspaceTarget(auth.codespaceUUID)
	if err != nil || !ok {
		_ = channel.Reject(ssh.ConnectionFailed, "workspace tcp target unavailable")
		return
	}
	backendConn, err := s.backend.OpenWorkspaceTCP(ctx, target.instanceName, payload.Port)
	if err != nil {
		_ = channel.Reject(ssh.ConnectionFailed, "workspace tcp target unavailable")
		return
	}
	clientChannel, requests, err := channel.Accept()
	if err != nil {
		_ = backendConn.Close()
		return
	}
	defer func() { _ = clientChannel.Close() }()
	notifyGatewaySSHActivity(activity)
	s.proxyWorkspaceStream(ctx, clientChannel, requests, backendConn, activity)
}

func (s *gatewaySSHServer) revalidateSession(ctx context.Context, auth gatewaySSHAuthContext, cancel context.CancelFunc) {
	ticker := time.NewTicker(s.revalidateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.controlPlane == nil {
				log.Printf("gateway ssh revalidate %s user %d: control plane is not ready", auth.codespaceUUID, auth.userID)
				cancel()
				return
			}
			decision, err := s.controlPlane.revalidateSSHSession(ctx, auth.userID, auth.codespaceUUID)
			if err != nil {
				log.Printf("gateway ssh revalidate %s user %d: %v", auth.codespaceUUID, auth.userID, err)
				cancel()
				return
			}
			if !decision.allowed {
				log.Printf("gateway ssh revalidate denied %s user %d: %s", auth.codespaceUUID, auth.userID, decision.deniedCategory)
				cancel()
				return
			}
		}
	}
}

func (s *gatewaySSHServer) serveWorkspaceSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, session provisioner.WorkspaceCommandSession, pty gatewaySSHPty, activity chan<- struct{}) {
	defer func() { _ = session.Close() }()
	if pty.enabled {
		_ = session.Resize(pty.cols, pty.rows)
	}
	stdinDone := make(chan struct{}, 1)
	stdoutDone := make(chan struct{}, 1)
	stderrDone := make(chan struct{}, 1)
	waitDone := make(chan error, 1)
	requestsDone := make(chan struct{}, 1)
	go func() {
		_ = copyGatewaySSHData(session.Stdin(), channel, activity)
		_ = session.Stdin().Close()
		stdinDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel, session.Stdout(), activity)
		stdoutDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel.Stderr(), session.Stderr(), activity)
		stderrDone <- struct{}{}
	}()
	go func() {
		waitDone <- session.Wait()
	}()
	go func() {
		s.handleWorkspaceSessionRequests(session, requests, pty.enabled, activity)
		requestsDone <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case err := <-waitDone:
		waitGatewaySSHChannelDone(stdoutDone)
		waitGatewaySSHChannelDone(stderrDone)
		status := gatewaySSHExitStatus(err)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
	case <-requestsDone:
	}
	_ = channel.Close()
	waitGatewaySSHChannelDone(stdinDone)
	waitGatewaySSHChannelDone(stdoutDone)
	waitGatewaySSHChannelDone(stderrDone)
}

func (s *gatewaySSHServer) proxyWorkspaceStream(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn io.ReadWriteCloser, activity chan<- struct{}) {
	requestsDone := make(chan struct{}, 1)
	go func() {
		s.rejectWorkspaceSessionRequests(requests, activity)
		requestsDone <- struct{}{}
	}()
	clientToBackendDone := make(chan struct{}, 1)
	backendToClientDone := make(chan struct{}, 1)
	go func() {
		_ = copyGatewaySSHData(conn, channel, activity)
		clientToBackendDone <- struct{}{}
	}()
	go func() {
		_ = copyGatewaySSHData(channel, conn, activity)
		backendToClientDone <- struct{}{}
	}()
	select {
	case <-ctx.Done():
	case <-requestsDone:
	case <-backendToClientDone:
	case <-clientToBackendDone:
		select {
		case <-ctx.Done():
		case <-requestsDone:
		case <-backendToClientDone:
		}
	}
	_ = channel.Close()
	_ = conn.Close()
	waitGatewaySSHChannelDone(clientToBackendDone)
	waitGatewaySSHChannelDone(backendToClientDone)
}

func (s *gatewaySSHServer) handleWorkspaceSessionRequests(session provisioner.WorkspaceCommandSession, requests <-chan *ssh.Request, ptyEnabled bool, activity chan<- struct{}) {
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		switch request.Type {
		case "window-change":
			if ptyEnabled {
				pty := parseGatewaySSHWindowChange(request.Payload, gatewaySSHPty{})
				_ = session.Resize(pty.cols, pty.rows)
			}
		case "signal":
			signal, ok := parseGatewaySSHSignal(request.Payload)
			if ok {
				ok = session.Signal(signal) == nil
			}
			if request.WantReply {
				_ = request.Reply(ok, nil)
			}
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func (s *gatewaySSHServer) rejectWorkspaceSessionRequests(requests <-chan *ssh.Request, activity chan<- struct{}) {
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

func waitGatewaySSHChannelDone(done <-chan struct{}) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func copyGatewaySSHData(dst io.Writer, src io.Reader, activity chan<- struct{}) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			notifyGatewaySSHActivity(activity)
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func parseGatewaySSHPty(payload []byte) gatewaySSHPty {
	var request struct {
		Term     string
		Cols     uint32
		Rows     uint32
		Width    uint32
		Height   uint32
		Modelist string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return gatewaySSHPty{enabled: true, cols: defaultGatewaySSHCols, rows: defaultGatewaySSHRows}
	}
	cols := int(request.Cols)
	rows := int(request.Rows)
	if cols <= 0 {
		cols = defaultGatewaySSHCols
	}
	if rows <= 0 {
		rows = defaultGatewaySSHRows
	}
	return gatewaySSHPty{enabled: true, cols: cols, rows: rows}
}

func parseGatewaySSHWindowChange(payload []byte, fallback gatewaySSHPty) gatewaySSHPty {
	var request struct {
		Cols   uint32
		Rows   uint32
		Width  uint32
		Height uint32
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return fallback
	}
	cols := int(request.Cols)
	rows := int(request.Rows)
	if cols <= 0 {
		cols = fallback.cols
	}
	if rows <= 0 {
		rows = fallback.rows
	}
	if cols <= 0 {
		cols = defaultGatewaySSHCols
	}
	if rows <= 0 {
		rows = defaultGatewaySSHRows
	}
	return gatewaySSHPty{enabled: true, cols: cols, rows: rows}
}

func parseGatewaySSHExecCommand(payload []byte) string {
	var request struct {
		Command string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return ""
	}
	return request.Command
}

func parseGatewaySSHSubsystem(payload []byte) string {
	var request struct {
		Name string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return ""
	}
	return request.Name
}

func parseGatewaySSHSignal(payload []byte) (int, bool) {
	var request struct {
		Signal string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return 0, false
	}
	switch strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(request.Signal)), "SIG") {
	case "ABRT":
		return int(syscall.SIGABRT), true
	case "ALRM":
		return int(syscall.SIGALRM), true
	case "FPE":
		return int(syscall.SIGFPE), true
	case "HUP":
		return int(syscall.SIGHUP), true
	case "ILL":
		return int(syscall.SIGILL), true
	case "INT":
		return int(syscall.SIGINT), true
	case "KILL":
		return int(syscall.SIGKILL), true
	case "PIPE":
		return int(syscall.SIGPIPE), true
	case "QUIT":
		return int(syscall.SIGQUIT), true
	case "SEGV":
		return int(syscall.SIGSEGV), true
	case "TERM":
		return int(syscall.SIGTERM), true
	case "USR1":
		return int(syscall.SIGUSR1), true
	case "USR2":
		return int(syscall.SIGUSR2), true
	default:
		return 0, false
	}
}

func gatewaySSHExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *provisioner.WorkspaceCommandExitError
	if errors.As(err, &exitErr) {
		if exitErr.Status >= 0 && exitErr.Status <= 255 {
			return exitErr.Status
		}
	}
	return 255
}

func notifyGatewaySSHActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}

func gatewaySSHAuthFromPermissions(permissions *ssh.Permissions) (gatewaySSHAuthContext, bool) {
	if permissions == nil {
		return gatewaySSHAuthContext{}, false
	}
	codespaceUUID := permissions.Extensions["codespace_uuid"]
	userID, err := strconv.ParseInt(strings.TrimSpace(permissions.Extensions["user_id"]), 10, 64)
	if err != nil || codespaceUUID == "" || userID <= 0 {
		return gatewaySSHAuthContext{}, false
	}
	return gatewaySSHAuthContext{codespaceUUID: codespaceUUID, userID: userID}, true
}

func codespaceUUIDFromGatewaySSHUser(user string) (string, bool) {
	codespaceUUID, ok := strings.CutPrefix(user, gatewaySSHUserPrefix)
	if !ok {
		return "", false
	}
	if err := validateCodespaceStateUUID(codespaceUUID); err != nil {
		return "", false
	}
	return codespaceUUID, true
}

func serveSSH(ctx context.Context, errorChannel chan<- error, listener net.Listener, server *gatewaySSHServer) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) && ctx.Err() != nil {
				return
			}
			errorChannel <- fmt.Errorf("gateway ssh listener: %w", err)
			return
		}
		if server == nil {
			_ = conn.Close()
			continue
		}
		go server.serveConn(ctx, conn)
	}
}
