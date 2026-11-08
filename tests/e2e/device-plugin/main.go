// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Command device-plugin advertises interchangeable FUSE allocations for the
// Kubernetes E2E suite. Each allocation makes the next Pod use the other ID.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	devicePluginDir     = "/var/lib/kubelet/device-plugins"
	devicePluginSocket  = devicePluginDir + "/codespace-e2e-fuse.sock"
	kubeletSocket       = devicePluginDir + "/kubelet.sock"
	resourceName        = "e2e.codespace.gitea.dev/fuse"
	deviceIDEnvironment = "E2E_FUSE_DEVICE_IDS"
)

type fusePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	mu      sync.Mutex
	health  map[string]string
	updated chan struct{}
}

func (*fusePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (plugin *fusePlugin) ListAndWatch(_ *pluginapi.Empty, stream grpc.ServerStreamingServer[pluginapi.ListAndWatchResponse]) error {
	for {
		plugin.mu.Lock()
		devices := []*pluginapi.Device{{ID: "fuse0", Health: plugin.health["fuse0"]}, {ID: "fuse1", Health: plugin.health["fuse1"]}}
		plugin.mu.Unlock()
		if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: devices}); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-plugin.updated:
		}
	}
}

func (plugin *fusePlugin) Allocate(_ context.Context, request *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	response := &pluginapi.AllocateResponse{ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, 0, len(request.ContainerRequests))}
	for _, container := range request.ContainerRequests {
		if len(container.DevicesIds) != 1 || container.DevicesIds[0] != "fuse0" && container.DevicesIds[0] != "fuse1" {
			return nil, fmt.Errorf("expected one FUSE allocation, got %v", container.DevicesIds)
		}
		deviceID := container.DevicesIds[0]
		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Envs:       map[string]string{deviceIDEnvironment: deviceID},
			CdiDevices: []*pluginapi.CDIDevice{{Name: resourceName + "=" + deviceID}},
		})
		plugin.mu.Lock()
		plugin.health["fuse0"], plugin.health["fuse1"] = pluginapi.Healthy, pluginapi.Healthy
		plugin.health[deviceID] = pluginapi.Unhealthy
		plugin.mu.Unlock()
		select {
		case plugin.updated <- struct{}{}:
		default:
		}
	}
	return response, nil
}

func run(ctx context.Context) error {
	if err := os.MkdirAll(devicePluginDir, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(devicePluginSocket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", devicePluginSocket)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(devicePluginSocket)
	}()

	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, &fusePlugin{
		health:  map[string]string{"fuse0": pluginapi.Healthy, "fuse1": pluginapi.Healthy},
		updated: make(chan struct{}, 1),
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	registration, err := grpc.NewClient("unix://"+kubeletSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		server.Stop()
		return err
	}
	defer func() { _ = registration.Close() }()
	registerContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = pluginapi.NewRegistrationClient(registration).Register(registerContext, &pluginapi.RegisterRequest{
		Version: pluginapi.Version, Endpoint: filepath.Base(devicePluginSocket), ResourceName: resourceName,
	})
	if err != nil {
		server.Stop()
		return fmt.Errorf("register device plugin: %w", err)
	}
	log.Printf("registered %s with rotating FUSE allocations", resourceName)

	select {
	case <-ctx.Done():
		// Kubelet keeps ListAndWatch open for the plugin lifetime, so a graceful
		// stop would wait on the stream that shutdown itself must close.
		server.Stop()
		return nil
	case err := <-serveDone:
		return err
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
