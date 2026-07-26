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

type gatewayWorkspaceSFTPBackend interface {
	OpenWorkspaceSFTP(ctx context.Context, request provisioner.WorkspaceSFTPRequest) (io.ReadWriteCloser, error)
}

type gatewayWorkspaceBackend interface {
	gatewayWorkspaceCommandBackend
	gatewayWorkspaceSFTPBackend
}

type gatewaySSHServer struct {
	config             *ssh.ServerConfig
	state              gatewayWorkspaceTargetStore
	routes             *gatewayRouteStore
	backend            gatewayWorkspaceBackend
	controlPlane       *gatewayControlPlane
	sessions           *gatewaySessionRegistry
	access             *gatewayAccessController
	authLimiter        *gatewaySSHAuthLimiter
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
	routes *gatewayRouteStore,
	backend gatewayWorkspaceBackend,
	controlPlane *gatewayControlPlane,
	sessions *gatewaySessionRegistry,
	access *gatewayAccessController,
	gatewayConfig GatewayConfig,
) (*gatewaySSHServer, error) {
	if hostKey == nil {
		return nil, fmt.Errorf("gateway ssh host key is required")
	}
	idleTimeout := gatewayConfig.SessionIdleTimeout.ToStdlib()
	if idleTimeout <= 0 {
		idleTimeout = DefaultConfig().Gateway.SessionIdleTimeout.ToStdlib()
	}
	revalidateInterval := gatewayConfig.SessionRevalidateInterval.ToStdlib()
	if revalidateInterval <= 0 {
		revalidateInterval = defaultGatewaySessionRevalidateInterval
	}
	maxChannels := gatewayConfig.SSHMaxChannelsPerConnection
	if maxChannels <= 0 {
		maxChannels = DefaultConfig().Gateway.SSHMaxChannelsPerConnection
	}
	server := &gatewaySSHServer{
		state:              state,
		routes:             routes,
		backend:            backend,
		controlPlane:       controlPlane,
		sessions:           sessions,
		access:             access,
		authLimiter:        newGatewaySSHAuthLimiterFromConfig(gatewayConfig),
		sessionIdleTimeout: idleTimeout,
		revalidateInterval: revalidateInterval,
		maxChannels:        maxChannels,
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: server.authenticatePublicKey,
		ServerVersion:     "SSH-2.0-gitea-codespace",
	}
	config.AddHostKey(hostKey)
	server.config = config
	return server, nil
}

func (s *gatewaySSHServer) authenticatePublicKey(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
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
	decision, err := s.controlPlane.verifySSHPublicKey(context.Background(), codespaceUUID, key.Marshal())
	if err != nil {
		return nil, err
	}
	if !decision.allowed {
		s.authLimiter.RecordFailure(sourceIP, codespaceUUID, publicKeyHash, decision.deniedCategory, time.Now())
		return nil, fmt.Errorf("ssh public key denied: %s", decision.deniedCategory)
	}
	return &ssh.Permissions{
		Extensions: map[string]string{
			"codespace_uuid": codespaceUUID,
			"user_id":        formatInt64(decision.userID),
		},
	}, nil
}

func (s *gatewaySSHServer) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	releaseTransport, ok := s.reserveTransport()
	if !ok {
		return
	}
	defer releaseTransport()
	sshConn, channels, requests, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		log.Printf("gateway ssh handshake: %v", err)
		return
	}
	defer sshConn.Close()
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
	activity := make(chan struct{}, 1)
	go s.revalidateSession(sessionCtx, auth, cancel)
	go s.cancelIdleSession(sessionCtx, cancel, activity)
	channelSlots := make(chan struct{}, s.maxChannels)

	for channel := range channels {
		channel := channel
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
	defer clientChannel.Close()

	target, ok, err := s.loadWorkspaceTarget(auth.codespaceUUID)
	if err != nil {
		_ = clientChannel.Close()
		return
	}
	if !ok {
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
			session, err := s.backend.OpenWorkspaceCommand(ctx, provisioner.WorkspaceCommandRequest{
				InstanceName: target.instanceName,
				Workdir:      target.workdir,
				User:         target.uid,
				Group:        target.gid,
				Command:      command,
				Interactive:  pty.enabled || request.Type == "shell",
				Cols:         pty.cols,
				Rows:         pty.rows,
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
			s.serveWorkspaceSFTP(ctx, clientChannel, requests, conn, activity)
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

type gatewaySSHDirectTCPIPPayload struct {
	Host           string
	Port           uint32
	OriginatorHost string
	OriginatorPort uint32
}

func (s *gatewaySSHServer) handleDirectTCPIP(ctx context.Context, auth gatewaySSHAuthContext, channel ssh.NewChannel, activity chan<- struct{}) {
	payload := gatewaySSHDirectTCPIPPayload{}
	if err := ssh.Unmarshal(channel.ExtraData(), &payload); err != nil ||
		strings.TrimSpace(payload.Host) == "" ||
		payload.Port == 0 ||
		payload.Port > 65535 {
		_ = channel.Reject(ssh.Prohibited, "invalid direct-tcpip target")
		return
	}
	if s.routes == nil {
		_ = channel.Reject(ssh.ConnectionFailed, "gateway endpoint route unavailable")
		return
	}
	route, proxyCtx, releaseRoute, ok := s.routes.BeginSSHDirectTCPIP(ctx, auth.codespaceUUID, strings.TrimSpace(payload.Host), payload.Port)
	if !ok {
		_ = channel.Reject(ssh.ConnectionFailed, "gateway endpoint route unavailable")
		return
	}
	defer releaseRoute()

	backendConn, err := (&net.Dialer{}).DialContext(proxyCtx, "tcp", route.upstreamHost)
	if err != nil {
		_ = channel.Reject(ssh.ConnectionFailed, "gateway endpoint route unavailable")
		return
	}
	clientChannel, requests, err := channel.Accept()
	if err != nil {
		_ = backendConn.Close()
		return
	}
	defer clientChannel.Close()
	defer backendConn.Close()
	notifyGatewaySSHActivity(activity)
	s.serveDirectTCPIP(proxyCtx, clientChannel, requests, backendConn, activity)
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
	defer session.Close()
	_ = session.Resize(pty.cols, pty.rows)
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
		s.handleWorkspaceSessionRequests(session, requests, activity)
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

func (s *gatewaySSHServer) serveWorkspaceSFTP(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn io.ReadWriteCloser, activity chan<- struct{}) {
	defer conn.Close()
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
	waitGatewaySSHChannelDone(clientToBackendDone)
	waitGatewaySSHChannelDone(backendToClientDone)
}

func (s *gatewaySSHServer) serveDirectTCPIP(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn net.Conn, activity chan<- struct{}) {
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

func (s *gatewaySSHServer) handleWorkspaceSessionRequests(session provisioner.WorkspaceCommandSession, requests <-chan *ssh.Request, activity chan<- struct{}) {
	for request := range requests {
		notifyGatewaySSHActivity(activity)
		switch request.Type {
		case "window-change":
			pty := parseGatewaySSHWindowChange(request.Payload, gatewaySSHPty{})
			_ = session.Resize(pty.cols, pty.rows)
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
	userID, err := parseInt64(permissions.Extensions["user_id"])
	if err != nil || codespaceUUID == "" || userID <= 0 {
		return gatewaySSHAuthContext{}, false
	}
	return gatewaySSHAuthContext{codespaceUUID: codespaceUUID, userID: userID}, true
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

func codespaceUUIDFromGatewaySSHUser(user string) (string, bool) {
	value, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(user)), gatewaySSHUserPrefix)
	if !ok || len(value) != 32 {
		return "", false
	}
	codespaceUUID := value[0:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:32]
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
