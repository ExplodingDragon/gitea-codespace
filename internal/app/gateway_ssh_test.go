// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
)

func TestGatewaySSHProxiesSessionToWorkspaceCommand(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	gatewayHostKey := newTestSSHSigner(t)

	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)

	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	gatewayConfig := DefaultConfig().Gateway
	backend := newTestWorkspaceCommandBackend("internal ready\n")
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		backend,
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(gatewayConfig),
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close()
	output, err := session.Output("echo ready")
	if err != nil {
		t.Fatalf("run command through gateway: %v", err)
	}
	if string(output) != "internal ready\n" {
		t.Fatalf("command output = %q", output)
	}
	if service.sshRequest == nil || service.sshRequest.GetCodespaceUuid() != codespaceUUID {
		t.Fatalf("verify ssh request = %#v", service.sshRequest)
	}
	if request := backend.lastRequest(); request.Interactive {
		t.Fatalf("exec without pty request opened an interactive tty")
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHSFTPUsesWorkspaceBackend(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	gatewayHostKey := newTestSSHSigner(t)
	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)
	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		newTestWorkspaceCommandBackend(""),
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(DefaultConfig().Gateway),
		DefaultConfig().Gateway,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("open gateway sftp: %v", err)
	}
	defer sftpClient.Close()

	if err := sftpClient.Mkdir("/dir"); err != nil {
		t.Fatalf("mkdir over gateway sftp: %v", err)
	}
	file, err := sftpClient.Create("/dir/file.txt")
	if err != nil {
		t.Fatalf("create file over gateway sftp: %v", err)
	}
	if _, err := file.Write([]byte("sftp ready")); err != nil {
		t.Fatalf("write file over gateway sftp: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close sftp file: %v", err)
	}
	opened, err := sftpClient.Open("/dir/file.txt")
	if err != nil {
		t.Fatalf("open file over gateway sftp: %v", err)
	}
	content, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil {
		t.Fatalf("read file over gateway sftp: %v", err)
	}
	if string(content) != "sftp ready" {
		t.Fatalf("sftp content = %q", content)
	}
	if err := sftpClient.Rename("/dir/file.txt", "/dir/renamed.txt"); err != nil {
		t.Fatalf("rename file over gateway sftp: %v", err)
	}
	entries, err := sftpClient.ReadDir("/dir")
	if err != nil {
		t.Fatalf("list dir over gateway sftp: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "renamed.txt" {
		t.Fatalf("sftp entries = %#v", entries)
	}
	if err := sftpClient.Remove("/dir/renamed.txt"); err != nil {
		t.Fatalf("remove file over gateway sftp: %v", err)
	}
	if err := sftpClient.RemoveDirectory("/dir"); err != nil {
		t.Fatalf("remove dir over gateway sftp: %v", err)
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHDirectTCPIPUsesRuntimeLoopback(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	gatewayHostKey := newTestSSHSigner(t)
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen runtime backend: %v", err)
	}
	defer backendListener.Close()
	backendDone := make(chan error, 3)
	go func() {
		for range 3 {
			conn, err := backendListener.Accept()
			if err != nil {
				backendDone <- err
				return
			}
			buffer := make([]byte, len("ping"))
			if _, err := io.ReadFull(conn, buffer); err != nil {
				_ = conn.Close()
				backendDone <- err
				return
			}
			if string(buffer) != "ping" {
				_ = conn.Close()
				backendDone <- io.ErrUnexpectedEOF
				return
			}
			_, err = conn.Write([]byte("pong"))
			_ = conn.Close()
			backendDone <- err
		}
	}()

	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)
	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		newTestWorkspaceCommandBackend(""),
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(DefaultConfig().Gateway),
		DefaultConfig().Gateway,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()

	_, backendPort, err := net.SplitHostPort(backendListener.Addr().String())
	if err != nil {
		t.Fatalf("parse backend address: %v", err)
	}
	for _, targetHost := range []string{"localhost", "127.0.0.1", "::1"} {
		conn, err := client.Dial("tcp", net.JoinHostPort(targetHost, backendPort))
		if err != nil {
			t.Fatalf("open direct-tcpip route for %s: %v", targetHost, err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			_ = conn.Close()
			t.Fatalf("write direct-tcpip for %s: %v", targetHost, err)
		}
		response := make([]byte, len("pong"))
		if _, err := io.ReadFull(conn, response); err != nil {
			_ = conn.Close()
			t.Fatalf("read direct-tcpip for %s: %v", targetHost, err)
		}
		if string(response) != "pong" {
			_ = conn.Close()
			t.Fatalf("direct-tcpip response for %s = %q", targetHost, response)
		}
		if err := conn.Close(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("close direct-tcpip for %s: %v", targetHost, err)
		}
		select {
		case err := <-backendDone:
			if err != nil {
				t.Fatalf("runtime backend for %s: %v", targetHost, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("runtime backend for %s was not reached", targetHost)
		}
	}
	if blocked, err := client.Dial("tcp", net.JoinHostPort("other-host", backendPort)); err == nil {
		_ = blocked.Close()
		t.Fatalf("expected non-loopback direct-tcpip target to fail")
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHClosesIdleTransport(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	gatewayHostKey := newTestSSHSigner(t)

	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)

	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	registry := newGatewaySessionRegistry()
	gatewayConfig := DefaultConfig().Gateway
	gatewayConfig.Sessions.IdleTimeout = Duration(50 * time.Millisecond)
	gatewayConfig.Sessions.RevalidateInterval = Duration(time.Hour)
	backend := newTestWorkspaceCommandBackend("")
	backend.block = true
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		backend,
		controlPlane,
		registry,
		newGatewayAccessControllerFromConfig(gatewayConfig),
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close()
	if err := session.Start("sleep"); err != nil {
		t.Fatalf("start command through gateway: %v", err)
	}
	if err := session.Signal(ssh.SIGINT); err != nil {
		t.Fatalf("signal command through gateway: %v", err)
	}
	select {
	case signal := <-backend.signal:
		if signal != int(syscall.SIGINT) {
			t.Fatalf("workspace signal = %d", signal)
		}
	case <-time.After(time.Second):
		t.Fatalf("workspace signal was not delivered")
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- session.Wait()
	}()
	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatalf("expected idle transport to close session")
		}
	case <-time.After(time.Second):
		t.Fatalf("idle transport did not close")
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if registry.LiveSessions(codespaceUUID) == 0 {
			cancel()
			_ = listener.Close()
			assertNoListenerError(t, errorChannel)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("live sessions after idle close = %d", registry.LiveSessions(codespaceUUID))
}

func TestGatewaySSHRejectsChannelsOverLimit(t *testing.T) {
	t.Parallel()

	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	gatewayHostKey := newTestSSHSigner(t)

	store := NewCodespaceStateStore(t.TempDir())
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InstanceName:       "cs-11111111111141118111",
		Workdir:            "/workspaces/repo",
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 1,
			Stage:             manager.RuntimeBootStageReady,
			StartedUnix:       100,
			LastUpdateUnix:    100,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata: %v", err)
	}
	saveGatewayWorkspaceIdentityForTest(t, store, codespaceUUID)

	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Allowed{
				Allowed: &codespacev1.SSHAuthBinding{UserId: 42},
			},
		},
		revalidateResponse: &codespacev1.RevalidateGatewaySessionResponse{
			Outcome: &codespacev1.RevalidateGatewaySessionResponse_Allowed{
				Allowed: &codespacev1.SessionAllowed{},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	gatewayConfig := DefaultConfig().Gateway
	gatewayConfig.SSH.MaxChannelsPerConnection = 1
	backend := newTestWorkspaceCommandBackend("")
	backend.block = true
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		store,
		backend,
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(gatewayConfig),
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway ssh: %v", err)
	}
	defer client.Close()
	first, err := client.NewSession()
	if err != nil {
		t.Fatalf("open first session: %v", err)
	}
	defer first.Close()
	if err := first.Start("sleep"); err != nil {
		t.Fatalf("start first command: %v", err)
	}
	if second, err := client.NewSession(); err == nil {
		_ = second.Close()
		t.Fatalf("expected second session to exceed channel limit")
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHRejectsTransportWhenGlobalInflightFull(t *testing.T) {
	t.Parallel()

	gatewayHostKey := newTestSSHSigner(t)
	gatewayConfig := DefaultConfig().Gateway
	gatewayConfig.Limits.MaxInflightTotal = 1
	access := newGatewayAccessControllerFromConfig(gatewayConfig)
	reservation, status := access.reserveRequest()
	if status != 0 {
		t.Fatalf("reserve existing request status = %d", status)
	}
	defer reservation.Release()
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		nil,
		nil,
		nil,
		nil,
		access,
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatalf("expected gateway ssh transport to be rejected")
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHClosesIncompleteHandshake(t *testing.T) {
	t.Parallel()

	gatewayConfig := DefaultConfig().Gateway
	gatewayConfig.SSH.HandshakeTimeout = Duration(50 * time.Millisecond)
	gatewayServer, err := newGatewaySSHServer(
		newTestSSHSigner(t),
		nil,
		nil,
		nil,
		nil,
		newGatewayAccessControllerFromConfig(gatewayConfig),
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial raw gateway ssh: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set raw ssh read deadline: %v", err)
	}
	buffer := make([]byte, 128)
	for {
		_, err = conn.Read(buffer)
		if err != nil {
			break
		}
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("incomplete ssh handshake remained open")
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHAuthLimiterBlocksBeforeControlPlane(t *testing.T) {
	t.Parallel()

	gatewayHostKey := newTestSSHSigner(t)
	service := &gatewayManagerService{
		sshResponse: &codespacev1.VerifySSHPublicKeyResponse{
			Outcome: &codespacev1.VerifySSHPublicKeyResponse_Denied{
				Denied: &codespacev1.FailureDetail{Category: "invalid_credentials"},
			},
		},
	}
	controlPlane, closeControlPlane := newTestGatewayControlPlane(t, service)
	defer closeControlPlane()
	gatewayConfig := DefaultConfig().Gateway
	gatewayServer, err := newGatewaySSHServer(
		gatewayHostKey,
		nil,
		nil,
		controlPlane,
		newGatewaySessionRegistry(),
		newGatewayAccessControllerFromConfig(gatewayConfig),
		gatewayConfig,
	)
	if err != nil {
		t.Fatalf("create gateway ssh server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway ssh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener, gatewayServer)
	defer listener.Close()

	clientKey := newTestSSHSigner(t)
	dialGatewaySSHExpectFailure(t, listener.Addr().String(), clientKey)
	dialGatewaySSHExpectFailure(t, listener.Addr().String(), clientKey)

	service.mu.Lock()
	sshCalls := service.sshCalls
	service.mu.Unlock()
	if sshCalls != 1 {
		t.Fatalf("verify ssh calls = %d", sshCalls)
	}
	cancel()
	_ = listener.Close()
	assertNoListenerError(t, errorChannel)
}

func TestGatewaySSHHostKeyPersistsInStateDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := loadOrCreateGatewaySSHHostKey(dir)
	if err != nil {
		t.Fatalf("load first host key: %v", err)
	}
	second, err := loadOrCreateGatewaySSHHostKey(dir)
	if err != nil {
		t.Fatalf("load second host key: %v", err)
	}
	if first.fingerprintSHA256 == "" || first.fingerprintSHA256 != second.fingerprintSHA256 {
		t.Fatalf("fingerprints = %q %q", first.fingerprintSHA256, second.fingerprintSHA256)
	}
	info, err := os.Stat(dir + "/" + gatewaySSHHostKeyFileName)
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("host key mode = %o", mode)
	}
}

func dialGatewaySSHExpectFailure(t *testing.T, address string, clientKey ssh.Signer) {
	t.Helper()
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            "cs-11111111-1111-4111-8111-111111111111",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatalf("expected gateway ssh authentication failure")
	}
}

func newTestSSHSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("create ssh signer: %v", err)
	}
	return signer
}
