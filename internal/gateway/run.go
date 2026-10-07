// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	componentv1 "gitea.dev/codespace-proto-go/component/v1"
	"gitea.dev/codespace-proto-go/component/v1/componentv1connect"
	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// Run serves the external HTTP and SSH protocols while Manager supplies
// configuration, authorization and current Runtime targets.
func Run(ctx context.Context, output io.Writer, client componentv1connect.ComponentServiceClient, certificateDirectory string) error {
	if client == nil || output == nil {
		return fmt.Errorf("gateway dependencies are incomplete")
	}
	var runtime *runningGateway
	var config *componentv1.GatewayConfigurationSnapshot
	sessionID := uuid.NewString()
	var sequence uint64
	var responseSequence uint64
	defer func() {
		if runtime != nil {
			runtime.stop()
		}
	}()
	for ctx.Err() == nil {
		streamCtx, cancelStream := context.WithCancel(ctx)
		stream := client.GatewayControl(streamCtx)
		sendActivity := func() error {
			sequence++
			return stream.Send(&componentv1.GatewayControlRequest{ProtocolVersion: 1, SessionId: sessionID, Sequence: sequence, ActiveSessions: gatewayActivity(runtime)})
		}
		if err := sendActivity(); err != nil {
			cancelStream()
			_ = stream.CloseRequest()
			_ = stream.CloseResponse()
			if !waitGatewayReconnect(ctx, runtime) {
				break
			}
			continue
		}
		responses := make(chan *componentv1.GatewayControlResponse)
		errors := make(chan error, 1)
		go func() {
			for {
				response, err := stream.Receive()
				if err != nil {
					errors <- err
					return
				}
				select {
				case responses <- response:
				case <-streamCtx.Done():
					return
				}
			}
		}()
		heartbeat := time.NewTicker(10 * time.Second)
		permit := time.NewTimer(30 * time.Second)
		connected := true
		var terminalErr error
		for ctx.Err() == nil {
			select {
			case response := <-responses:
				if response.ProtocolVersion != 1 || response.Sequence < responseSequence || response.Sequence > sequence || response.PermitValidForMilliseconds <= 0 {
					terminalErr = fmt.Errorf("manager returned an invalid Gateway control response")
					connected = false
					break
				}
				responseSequence = response.Sequence
				if !permit.Stop() {
					select {
					case <-permit.C:
					default:
					}
				}
				permit.Reset(time.Duration(response.PermitValidForMilliseconds) * time.Millisecond)
				if !response.Snapshot {
					continue
				}
				if response.Config == nil || response.Cursor == "" {
					terminalErr = fmt.Errorf("manager returned an incomplete Gateway snapshot")
					connected = false
					break
				}
				if runtime != nil && proto.Equal(config, response.Config) {
					if err := runtime.routes.ReplaceSharedGatewayRuntimes(gatewayRuntimes(response.Runtimes)); err != nil {
						terminalErr = err
						connected = false
						break
					}
					runtime.health.Recover()
					continue
				}
				if runtime != nil {
					runtime.stop()
				}
				started, err := startGateway(ctx, output, client, certificateDirectory, response)
				if err != nil {
					terminalErr = err
					connected = false
					break
				}
				runtime = started
				config = response.Config
			case <-heartbeat.C:
				if err := sendActivity(); err != nil {
					connected = false
				}
			case <-gatewayActivityChanges(runtime):
				if err := sendActivity(); err != nil {
					connected = false
				}
			case <-permit.C:
				connected = false
			case <-errors:
				connected = false
			case <-ctx.Done():
				connected = false
			}
			if !connected {
				break
			}
		}
		heartbeat.Stop()
		if !permit.Stop() {
			select {
			case <-permit.C:
			default:
			}
		}
		cancelStream()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		if terminalErr != nil {
			return terminalErr
		}
		if !waitGatewayReconnect(ctx, runtime) {
			break
		}
	}
	return nil
}

func gatewayActivityChanges(runtime *runningGateway) <-chan struct{} {
	if runtime == nil || runtime.sessions == nil {
		return nil
	}
	return runtime.sessions.ActivityChanges()
}

func gatewayActivity(runtime *runningGateway) map[string]int64 {
	counts := make(map[string]int64)
	if runtime == nil || runtime.sessions == nil {
		return counts
	}
	for runtimeUUID, count := range runtime.sessions.Activity(time.Now()) {
		counts[runtimeUUID] = int64(count)
	}
	return counts
}

func waitGatewayReconnect(ctx context.Context, runtime *runningGateway) bool {
	if runtime != nil {
		runtime.health.Warn()
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Second):
		return true
	}
}

type runningGateway struct {
	cancel   context.CancelFunc
	http     *http.Server
	httpLn   net.Listener
	sshLn    net.Listener
	routes   *RouteStore
	sessions *SessionRegistry
	health   *ProcessHealth
	done     chan struct{}
	stopOnce sync.Once
}

func startGateway(parent context.Context, output io.Writer, client componentv1connect.ComponentServiceClient, certificateDirectory string, snapshot *componentv1.GatewayControlResponse) (*runningGateway, error) {
	if snapshot == nil || snapshot.Config == nil || snapshot.Cursor == "" {
		return nil, fmt.Errorf("gateway snapshot is incomplete")
	}
	config := gatewayConfig(snapshot.Config)
	hostKey, err := os.ReadFile("/var/run/codespace/gateway/ssh-host-key")
	if err != nil {
		return nil, fmt.Errorf("read Gateway SSH host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		return nil, fmt.Errorf("parse Gateway SSH host key: %w", err)
	}
	httpLn, err := net.Listen("tcp", config.HTTP.Listen)
	if err != nil {
		return nil, err
	}
	sshLn, err := net.Listen("tcp", config.SSH.Listen)
	if err != nil {
		_ = httpLn.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	backend := &AgentBackend{Manager: client, CertificateDirectory: certificateDirectory}
	routes := NewRouteStore(backend)
	sessions := NewSessionRegistryFromConfig(config)
	routes.SetSessionRegistry(sessions)
	access := NewAccessControllerFromConfig(config)
	control := &ComponentControlPlane{Client: client}
	origin, err := NewOriginPolicy(config.HTTP.PublicURL)
	if err != nil {
		cancel()
		_ = httpLn.Close()
		_ = sshLn.Close()
		return nil, err
	}
	if err := routes.ReplaceSharedGatewayRuntimes(gatewayRuntimes(snapshot.Runtimes)); err != nil {
		cancel()
		_ = httpLn.Close()
		_ = sshLn.Close()
		return nil, err
	}
	health := NewProcessHealth()
	httpServer := &http.Server{
		Handler:           NewHandler(health, sessions, access, control, origin, NewBrowserAuth(routes), routes),
		ReadHeaderTimeout: HTTPReadHeaderTime,
		MaxHeaderBytes:    HTTPMaxHeaderBytes,
	}
	sshServer, err := NewSSHServer(signer, routes, routes, control, sessions, access, config)
	if err != nil {
		cancel()
		routes.Close()
		_ = httpLn.Close()
		_ = sshLn.Close()
		return nil, err
	}
	runtime := &runningGateway{cancel: cancel, http: httpServer, httpLn: httpLn, sshLn: sshLn, routes: routes, sessions: sessions, health: health, done: make(chan struct{})}
	go func() {
		defer close(runtime.done)
		errorsCh := make(chan error, 2)
		go func() {
			if err := httpServer.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errorsCh <- err
			}
		}()
		go ServeSSH(ctx, errorsCh, sshLn, sshServer)
		select {
		case <-ctx.Done():
		case <-errorsCh:
			health.Fail()
			cancel()
		}
	}()
	_, _ = fmt.Fprintf(output, "codespace gateway HTTP listening on %s\ncodespace gateway SSH listening on %s\n", httpLn.Addr(), sshLn.Addr())
	return runtime, nil
}

func (r *runningGateway) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.cancel()
		r.routes.Close()
		_ = r.httpLn.Close()
		_ = r.sshLn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.http.Shutdown(ctx)
		select {
		case <-r.done:
		case <-ctx.Done():
		}
	})
}

func gatewayConfig(value *componentv1.GatewayConfigurationSnapshot) configpkg.GatewayConfig {
	config := configpkg.DefaultGatewayConfig()
	config.HTTP.Listen, config.HTTP.PublicURL = value.HttpListen, value.PublicUrl
	config.SSH.Listen, config.SSH.PublicAddr = value.SshListen, value.SshPublicAddress
	config.SSH.MaxChannelsPerConnection = int(value.MaxChannelsPerSshConnection)
	config.Sessions.TTL = configpkg.Duration(time.Duration(value.SessionTtlMilliseconds) * time.Millisecond)
	config.Sessions.IdleTimeout = configpkg.Duration(time.Duration(value.SessionIdleTimeoutMilliseconds) * time.Millisecond)
	config.Sessions.RevalidateInterval = configpkg.Duration(time.Duration(value.RevalidateIntervalMilliseconds) * time.Millisecond)
	config.Sessions.MaxPerCodespace, config.Sessions.MaxPerUser = int(value.MaxSessionsPerCodespace), int(value.MaxSessionsPerUser)
	config.Limits.MaxInflightTotal, config.Limits.MaxInflightPerSession = int(value.MaxInflight), int(value.MaxInflightPerSession)
	return config
}

func gatewayRuntimes(routes []*componentv1.GatewayRuntimeRoute) []RuntimeSnapshot {
	result := make([]RuntimeSnapshot, 0, len(routes))
	for _, route := range routes {
		if route == nil || !validAgentAddress(route.AgentAddress) {
			continue
		}
		item := RuntimeSnapshot{
			SiteUID: route.SiteUid, ResourceUID: route.ResourceUid, RuntimeUUID: route.RuntimeUuid,
			GiteaWebURL: route.GiteaWebUrl, PodUID: route.PodUid, AgentAddress: route.AgentAddress,
			TargetVersion: route.TargetVersion,
		}
		for _, endpoint := range route.Endpoints {
			if endpoint != nil {
				item.Endpoints = append(item.Endpoints, EndpointSnapshot{EndpointID: endpoint.EndpointId, Label: endpoint.Label, Public: endpoint.Public})
			}
		}
		result = append(result, item)
	}
	return result
}
