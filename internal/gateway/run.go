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
	"uuid"

	configpkg "gitea.dev/codespace/internal/config"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"gitea.dev/codespace/internal/transport"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

const gatewayInitialControlResponseTimeout = 30 * time.Second

// Run serves the external HTTP and SSH protocols while Manager supplies
// configuration, authorization and current Runtime targets.
func Run(ctx context.Context, output io.Writer, client componentv1connect.ComponentServiceClient, certificates *transport.FileCertificates) error {
	if client == nil || output == nil || certificates == nil {
		return fmt.Errorf("gateway dependencies are incomplete")
	}
	var runtime *runningGateway
	var config *componentv1.GatewayConfigurationSnapshot
	sessionID := uuid.NewV4().String()
	var sequence uint64
	var responseSequence uint64
	var permitUntil time.Time
	defer func() {
		if runtime != nil {
			runtime.stop()
		}
	}()
	for ctx.Err() == nil {
		if runtime != nil && !permitUntil.IsZero() && !time.Now().Before(permitUntil) {
			runtime.stop()
			runtime, config = nil, nil
		}
		streamCtx, cancelStream := context.WithCancel(ctx)
		stream, err := client.GatewayControl(streamCtx)
		if err != nil {
			cancelStream()
			if !waitGatewayReconnect(ctx, runtime) {
				break
			}
			continue
		}
		sendActivity := func() error {
			sequence++
			return stream.Send(&componentv1.GatewayControlRequest{ProtocolVersion: 1, SessionId: sessionID, Sequence: sequence, ActiveSessions: gatewayActivity(runtime)})
		}
		if err := sendActivity(); err != nil {
			cancelStream()
			_ = stream.CloseSend()
			_ = stream.Close()
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
		permit := time.NewTimer(time.Until(gatewayControlDeadline(runtime != nil, permitUntil, time.Now())))
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
				permitUntil = time.Now().Add(time.Duration(response.PermitValidForMilliseconds) * time.Millisecond)
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
				started, err := startGateway(ctx, output, client, certificates, response)
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
				if runtime != nil {
					runtime.stop()
					runtime, config = nil, nil
				}
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
		_ = stream.CloseSend()
		_ = stream.Close()
		if terminalErr != nil {
			return terminalErr
		}
		if !waitGatewayReconnect(ctx, runtime) {
			break
		}
	}
	return nil
}

func gatewayControlDeadline(runtimeActive bool, permitUntil, now time.Time) time.Time {
	if runtimeActive && !permitUntil.IsZero() {
		return permitUntil
	}
	return now.Add(gatewayInitialControlResponseTimeout)
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

func startGateway(parent context.Context, output io.Writer, client componentv1connect.ComponentServiceClient, certificates *transport.FileCertificates, snapshot *componentv1.GatewayControlResponse) (*runningGateway, error) {
	if snapshot == nil || snapshot.Config == nil || snapshot.Cursor == "" {
		return nil, fmt.Errorf("gateway snapshot is incomplete")
	}
	config, err := gatewayConfig(snapshot.Config)
	if err != nil {
		return nil, err
	}
	hostKey, err := os.ReadFile("/var/run/codespace/gateway/ssh-host-key")
	if err != nil {
		return nil, fmt.Errorf("read Gateway SSH host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		return nil, fmt.Errorf("parse Gateway SSH host key: %w", err)
	}
	sessionKey, err := os.ReadFile("/var/run/codespace/gateway/session-key")
	if err != nil {
		return nil, fmt.Errorf("read Gateway session key: %w", err)
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
	backend := &AgentBackend{Manager: client, Certificates: certificates}
	routes := NewRouteStore(backend)
	sessions, err := NewSessionRegistryFromConfig(config, sessionKey)
	if err != nil {
		cancel()
		routes.Close()
		_ = httpLn.Close()
		_ = sshLn.Close()
		return nil, fmt.Errorf("initialize Gateway sessions: %w", err)
	}
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
		Handler:           gatewayClientIPMiddleware(NewHandler(health, sessions, access, control, origin, NewBrowserAuth(routes), routes), config.HTTP.TrustedProxyCIDRs),
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

func gatewayConfig(value *componentv1.GatewayConfigurationSnapshot) (configpkg.GatewayConfig, error) {
	if value == nil {
		return configpkg.GatewayConfig{}, fmt.Errorf("gateway configuration is missing")
	}
	config := configpkg.GatewayConfig{
		HTTP: configpkg.GatewayHTTPConfig{
			Listen: value.HttpListen, PublicURL: value.PublicUrl,
			TrustedProxyCIDRs: append([]string(nil), value.TrustedProxyCidrs...),
		},
		SSH: configpkg.GatewaySSHConfig{
			Listen: value.SshListen, PublicAddr: value.SshPublicAddress,
			HandshakeTimeout:         configpkg.Duration(time.Duration(value.SshHandshakeTimeoutMilliseconds) * time.Millisecond),
			MaxChannelsPerConnection: int(value.MaxChannelsPerSshConnection),
			Auth: configpkg.GatewaySSHAuthConfig{
				MaxAttemptsPerIP:          int(value.SshAuthMaxAttemptsPerIp),
				MaxAttemptsPerCodespace:   int(value.SshAuthMaxAttemptsPerCodespace),
				MaxAttemptsPerIPCodespace: int(value.SshAuthMaxAttemptsPerIpCodespace),
				MaxAttemptsPerPublicKey:   int(value.SshAuthMaxAttemptsPerPublicKey),
				FailureWindow:             configpkg.Duration(time.Duration(value.SshAuthFailureWindowMilliseconds) * time.Millisecond),
			},
		},
		Sessions: configpkg.GatewaySessionConfig{
			TTL:                configpkg.Duration(time.Duration(value.SessionTtlMilliseconds) * time.Millisecond),
			IdleTimeout:        configpkg.Duration(time.Duration(value.SessionIdleTimeoutMilliseconds) * time.Millisecond),
			RevalidateInterval: configpkg.Duration(time.Duration(value.RevalidateIntervalMilliseconds) * time.Millisecond),
			MaxPerCodespace:    int(value.MaxSessionsPerCodespace), MaxPerUser: int(value.MaxSessionsPerUser),
		},
		Limits: configpkg.GatewayLimitsConfig{
			MaxInflightTotal: int(value.MaxInflight), MaxInflightPerSession: int(value.MaxInflightPerSession),
			PublicMaxConnectionsPerEndpoint: int(value.PublicMaxConnectionsPerEndpoint),
			PublicMaxConnectionsPerIP:       int(value.PublicMaxConnectionsPerIp),
			ValidationMaxInflight:           int(value.ValidationMaxInflight),
		},
	}
	if err := config.Validate(); err != nil {
		return configpkg.GatewayConfig{}, fmt.Errorf("invalid Gateway configuration snapshot: %w", err)
	}
	return config, nil
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
