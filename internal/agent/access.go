// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/accessticket"
	"gitea.dev/codespace/internal/devcontainerruntime"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// AccessServer exposes only the current Dev Container target. Kubernetes and
// lifecycle operations remain outside this service.
type AccessServer struct {
	agentv1connect.UnimplementedAgentAccessServiceHandler
	Runtime  *Runtime
	sessions chan struct{}
}

func NewAccessServer(runtime *Runtime) *AccessServer {
	return &AccessServer{Runtime: runtime, sessions: make(chan struct{}, 64)}
}

func (s *AccessServer) Handler() (string, http.Handler) {
	return agentv1connect.NewAgentAccessServiceHandler(s, connect.WithReadMaxBytes(256*1024), connect.WithSendMaxBytes(256*1024))
}

func (s *AccessServer) Access(ctx context.Context, stream *connect.BidiStream[agentv1.AccessRequest, agentv1.AccessResponse]) error {
	select {
	case s.sessions <- struct{}{}:
		defer func() { <-s.sessions }()
	default:
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("agent access session limit reached"))
	}
	request, err := stream.Receive()
	if err != nil {
		return err
	}
	open := request.GetOpen()
	if request.ProtocolVersion != 1 || open == nil || open.Ticket == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first access frame must open one capability"))
	}
	target, state, secrets, capability, endpoint, err := s.Runtime.authorize(open)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	if err := stream.Send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Accepted{Accepted: true}}); err != nil {
		return err
	}
	return runAccess(ctx, stream, state, secrets, target, capability, endpoint, open)
}

func (r *Runtime) authorize(open *agentv1.OpenAccess) (*agentv1.AccessTarget, *devcontainer.State, map[string]string, agentv1.AccessCapability, *agentv1.EndpointTarget, error) {
	capability := accessCapability(open)
	if capability == agentv1.AccessCapability_ACCESS_CAPABILITY_UNSPECIFIED {
		return nil, nil, nil, capability, nil, fmt.Errorf("access capability is invalid")
	}
	r.mu.RLock()
	var target *agentv1.AccessTarget
	if r.target != nil {
		target = proto.Clone(r.target).(*agentv1.AccessTarget)
	}
	secrets := maps.Clone(r.secrets)
	verificationKey := append([]byte(nil), r.verificationKey...)
	r.mu.RUnlock()
	if target == nil {
		return nil, nil, nil, capability, nil, fmt.Errorf("runtime access target is not ready")
	}
	claims, err := accessticket.Verify(ed25519.PublicKey(verificationKey), open.Ticket, time.Now())
	if err != nil {
		return nil, nil, nil, capability, nil, err
	}
	identity := r.Journal.record.Identity
	if claims.SiteUid != identity.SiteUID || claims.ResourceUid != identity.ResourceUID || claims.RuntimeUuid != identity.RuntimeUUID || claims.PodUid != r.PodUID || claims.TargetVersion != target.Version || claims.Capability != capability {
		return nil, nil, nil, capability, nil, fmt.Errorf("access ticket does not match the current runtime target")
	}
	var selected *agentv1.EndpointTarget
	if capability == agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT {
		requested := open.GetEndpoint()
		if requested == nil || requested.EndpointId != claims.EndpointId {
			return nil, nil, nil, capability, nil, fmt.Errorf("endpoint access does not match its ticket")
		}
		for _, item := range target.Endpoints {
			if item.GetEndpoint().GetEndpointId() == requested.EndpointId {
				selected = proto.Clone(item).(*agentv1.EndpointTarget)
				break
			}
		}
		if selected == nil {
			return nil, nil, nil, capability, nil, fmt.Errorf("endpoint is not in the current target")
		}
	}
	data, err := r.Journal.read("state/environment.json", 4*1024*1024)
	if err != nil {
		return nil, nil, nil, capability, nil, err
	}
	state := &devcontainer.State{}
	if err := json.Unmarshal(data, state); err != nil || state.Validate() != nil || state.OwnerID != identity.RuntimeUUID || state.PrimaryContainerID != target.PrimaryContainerId {
		return nil, nil, nil, capability, nil, fmt.Errorf("saved Dev Container does not match the access target")
	}
	return target, state, secrets, capability, selected, nil
}

func accessCapability(open *agentv1.OpenAccess) agentv1.AccessCapability {
	switch open.Capability.(type) {
	case *agentv1.OpenAccess_Terminal:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_TERMINAL
	case *agentv1.OpenAccess_Command:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND
	case *agentv1.OpenAccess_Sftp:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP
	case *agentv1.OpenAccess_Endpoint:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT
	case *agentv1.OpenAccess_Tcp:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_LOOPBACK_TCP
	default:
		return agentv1.AccessCapability_ACCESS_CAPABILITY_UNSPECIFIED
	}
}

func runAccess(ctx context.Context, stream *connect.BidiStream[agentv1.AccessRequest, agentv1.AccessResponse], state *devcontainer.State, secrets map[string]string, target *agentv1.AccessTarget, capability agentv1.AccessCapability, endpoint *agentv1.EndpointTarget, open *agentv1.OpenAccess) error {
	apiClient, err := client.NewClientWithOpts(client.WithHost(DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func() { _ = apiClient.Close() }()
	containerID := target.PrimaryContainerId
	command := []string{"/bin/sh", "-l"}
	interactive := false
	user, workdir := state.RemoteUser, state.RemoteWorkdir
	if capability == agentv1.AccessCapability_ACCESS_CAPABILITY_TERMINAL || capability == agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND || capability == agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP {
		engine, err := containerdocker.New(ctx, io.Discard, io.Discard)
		if err != nil {
			return err
		}
		err = engine.RunPostAttach(ctx, state, secrets)
		closeErr := engine.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	switch capability {
	case agentv1.AccessCapability_ACCESS_CAPABILITY_TERMINAL:
		terminal := open.GetTerminal()
		if terminal == nil || len(terminal.Term) > 64 || strings.ContainsFunc(terminal.Term, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("terminal type is invalid"))
		}
		interactive = true
	case agentv1.AccessCapability_ACCESS_CAPABILITY_COMMAND:
		requestedCommand := open.GetCommand()
		value := requestedCommand.GetCommand()
		if len(value) > 32*1024 || strings.ContainsRune(value, 0) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("command is invalid"))
		}
		command = []string{"/bin/sh", "-lc", value}
		if requestedCommand.Terminal != nil {
			if requestedCommand.Terminal.Term == "" || len(requestedCommand.Terminal.Term) > 64 || strings.ContainsFunc(requestedCommand.Terminal.Term, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("terminal type is invalid"))
			}
			interactive = true
		}
	case agentv1.AccessCapability_ACCESS_CAPABILITY_SFTP:
		command = []string{devcontainerruntime.ContainerRuntimeBinary, "runtime", "sftp", "--workdir", state.RemoteWorkdir}
	case agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT:
		containerID = endpoint.ContainerId
		command, user, workdir = []string{devcontainerruntime.ContainerRuntimeBinary, "runtime", "connect", "--host", "localhost", "--port", fmt.Sprint(endpoint.Port)}, "", "/"
	case agentv1.AccessCapability_ACCESS_CAPABILITY_LOOPBACK_TCP:
		port := open.GetTcp().GetPort()
		if port == 0 || port > 65535 {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("loopback TCP port is invalid"))
		}
		command = []string{devcontainerruntime.ContainerRuntimeBinary, "runtime", "connect", "--host", "localhost", "--port", fmt.Sprint(port)}
	default:
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("access capability is invalid"))
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets)
	terminal := open.GetTerminal()
	if terminal == nil && open.GetCommand() != nil {
		terminal = open.GetCommand().Terminal
	}
	if terminal != nil {
		values["TERM"] = terminal.Term
		values["COLORTERM"] = "truecolor"
	}
	options := container.ExecOptions{User: user, WorkingDir: workdir, Env: stringMapEnvironment(values), Cmd: command, AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: interactive}
	if terminal != nil && terminal.Size != nil && terminal.Size.Columns > 0 && terminal.Size.Rows > 0 {
		options.ConsoleSize = &[2]uint{uint(terminal.Size.Rows), uint(terminal.Size.Columns)}
	}
	created, err := apiClient.ContainerExecCreate(ctx, containerID, options)
	if err != nil {
		return err
	}
	attached, err := apiClient.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: interactive, ConsoleSize: options.ConsoleSize})
	if err != nil {
		return err
	}
	defer attached.Close()
	writeErr := make(chan error, 1)
	go func() {
		for {
			frame, err := stream.Receive()
			if err != nil {
				_ = attached.CloseWrite()
				writeErr <- err
				return
			}
			switch value := frame.Frame.(type) {
			case *agentv1.AccessRequest_Stdin:
				_, err = attached.Conn.Write(value.Stdin)
			case *agentv1.AccessRequest_Resize:
				if !interactive || value.Resize == nil || value.Resize.Columns == 0 || value.Resize.Rows == 0 {
					err = fmt.Errorf("terminal resize is invalid")
				} else {
					err = apiClient.ContainerExecResize(ctx, created.ID, container.ResizeOptions{Height: uint(value.Resize.Rows), Width: uint(value.Resize.Columns)})
				}
			case *agentv1.AccessRequest_Signal:
				if value.Signal <= 0 || value.Signal > 64 {
					err = fmt.Errorf("process signal is invalid")
				} else if inspect, inspectErr := apiClient.ContainerExecInspect(ctx, created.ID); inspectErr != nil || inspect.Pid <= 0 {
					err = errors.Join(inspectErr, fmt.Errorf("workspace process is not running"))
				} else {
					err = unix.Kill(inspect.Pid, syscall.Signal(value.Signal))
				}
			case *agentv1.AccessRequest_StdinEof:
				if !value.StdinEof {
					err = fmt.Errorf("stdin EOF frame is invalid")
				} else if interactive {
					// A PTY receives terminal EOF as EOT. Let the process finish
					// before closing the hijacked input so pending output is kept.
					_, _ = attached.Conn.Write([]byte{0x04})
					deadline := time.Now().Add(time.Second)
					for time.Now().Before(deadline) {
						inspect, inspectErr := apiClient.ContainerExecInspect(ctx, created.ID)
						if inspectErr != nil || !inspect.Running {
							break
						}
						select {
						case <-ctx.Done():
							err = ctx.Err()
						case <-time.After(10 * time.Millisecond):
						}
						if err != nil {
							break
						}
					}
					if err == nil {
						err = attached.CloseWrite()
					}
				} else {
					err = attached.CloseWrite()
				}
			default:
				err = fmt.Errorf("access frame is invalid after open")
			}
			if err != nil {
				writeErr <- err
				return
			}
		}
	}()
	send := func(response *agentv1.AccessResponse) error { return stream.Send(response) }
	var outputErr error
	if interactive {
		outputErr = copyFrames(attached.Reader, func(value []byte) error {
			return send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: value}})
		})
	} else {
		stdout := frameWriter{send: func(value []byte) error {
			return send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stdout{Stdout: value}})
		}}
		stderr := frameWriter{send: func(value []byte) error {
			return send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_Stderr{Stderr: value}})
		}}
		_, outputErr = stdcopy.StdCopy(stdout, stderr, attached.Reader)
	}
	result, inspectErr := apiClient.ContainerExecInspect(ctx, created.ID)
	if inspectErr == nil {
		inspectErr = send(&agentv1.AccessResponse{Frame: &agentv1.AccessResponse_ExitStatus{ExitStatus: int32(result.ExitCode)}})
	}
	select {
	case receiveErr := <-writeErr:
		if !errors.Is(receiveErr, io.EOF) && !errors.Is(receiveErr, context.Canceled) {
			return errors.Join(outputErr, inspectErr, receiveErr)
		}
	default:
	}
	return errors.Join(outputErr, inspectErr)
}

type frameWriter struct{ send func([]byte) error }

func (w frameWriter) Write(value []byte) (int, error) {
	if err := w.send(append([]byte(nil), value...)); err != nil {
		return 0, err
	}
	return len(value), nil
}

func copyFrames(reader io.Reader, send func([]byte) error) error {
	buffer := make([]byte, 32*1024)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			if sendErr := send(append([]byte(nil), buffer[:read]...)); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func stringMapEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}
