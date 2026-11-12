// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"golang.org/x/sys/unix"
)

const (
	USBIPDeviceRootPath = "/run/codespace/usbip/dev"
	USBIPDevptsPath     = "/run/codespace/usbip/devpts"
	usbIPCommandTimeout = 30 * time.Second
)

var usbIPBusIDPattern = regexp.MustCompile(`^[0-9]+-[0-9]+(?:\.[0-9]+)*$`)

// USBIPRuntime owns the device mounts and standard Linux USB/IP client.
type USBIPRuntime struct {
	enabled bool
	command string
}

// RuntimeUSBIPServer exposes USB/IP operations on the private Runtime socket.
type RuntimeUSBIPServer struct {
	agentv1connect.UnimplementedRuntimeUSBIPServiceHandler
	Runtime *Runtime
}

func (s *RuntimeUSBIPServer) Run(ctx context.Context, request *agentv1.RuntimeUSBIPServiceRunRequest) (*agentv1.RuntimeUSBIPServiceRunResponse, error) {
	if request == nil || request.ProtocolVersion != 1 || s.Runtime == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP request is invalid")
	}
	if !s.Runtime.USBIP.Enabled() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "USB/IP is unavailable because this Kata runtime has no VHCI controller")
	}
	response := &agentv1.RuntimeUSBIPServiceRunResponse{Sources: s.Runtime.forwards.sources()}
	var selected *agentv1.USBIPSource
	if request.Source != "" {
		for _, source := range response.Sources {
			if source.Name == request.Source {
				selected = source
				break
			}
		}
		if selected == nil {
			return nil, connect.NewError(connect.CodeNotFound, "USB/IP source is not registered")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, usbIPCommandTimeout)
	defer cancel()
	argument := strings.TrimSpace(request.Argument)
	var args []string
	switch request.Operation {
	case agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES:
		if argument != "" || request.Source != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP sources takes no arguments")
		}
		return response, nil
	case agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST:
		if argument != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP list takes no argument")
		}
		if selected != nil {
			response.Sources = []*agentv1.USBIPSource{selected}
		}
		// One deadline and bounded concurrency keep a failed source from delaying
		// every other source or hiding devices that were successfully listed.
		var wg sync.WaitGroup
		slots := make(chan struct{}, 4)
		for _, source := range response.Sources {
			wg.Go(func() {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					source.Error = ctx.Err().Error()
					return
				}
				output, err := s.Runtime.runUSBIPSource(ctx, source, "list")
				source.Output = output
				if err != nil {
					source.Error = err.Error()
				}
			})
		}
		wg.Wait()
		return response, nil
	case agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_ATTACH:
		if !usbIPBusIDPattern.MatchString(argument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP bus ID is invalid")
		}
		if selected == nil && len(response.Sources) == 1 {
			selected = response.Sources[0]
		}
		if selected == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, "select a registered USB/IP source with --source")
		}
		output, err := s.Runtime.runUSBIPSource(ctx, selected, "attach", "--busid="+argument)
		if err != nil {
			return nil, err
		}
		response.Output = output
		return response, nil
	case agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS:
		if argument != "" || request.Source != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP status takes no arguments")
		}
		args = []string{"port"}
	case agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_DETACH:
		port, err := strconv.ParseUint(argument, 10, 16)
		if err != nil || request.Source != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP port is invalid")
		}
		args = []string{"detach", "--port=" + strconv.FormatUint(port, 10)}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "USB/IP operation is invalid")
	}
	output, err := s.Runtime.USBIP.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	response.Output = output
	return response, nil
}

func (r *Runtime) runUSBIPSource(ctx context.Context, source *agentv1.USBIPSource, operation string, extra ...string) (string, error) {
	ctx, cancel, err := r.forwards.sourceContext(ctx, source)
	if err != nil {
		return "", err
	}
	defer cancel()
	args := []string{"--tcp-port", strconv.FormatUint(uint64(source.Port), 10), operation, "--remote=127.0.0.1"}
	return r.USBIP.run(ctx, append(args, extra...)...)
}

// PrepareUSBIP enables USB/IP only when a Kata Guest exposes an active VHCI
// controller. The device tree is a fresh devtmpfs mount so nodes created by the
// Guest kernel remain visible to a Dev Container after attach and detach.
func PrepareUSBIP(kata bool) (*USBIPRuntime, error) {
	runtime := &USBIPRuntime{}
	if !kata {
		return runtime, nil
	}
	controllers, err := filepath.Glob("/sys/devices/platform/vhci_hcd.*")
	if err != nil {
		return nil, fmt.Errorf("inspect USB/IP VHCI controllers: %w", err)
	}
	if len(controllers) == 0 {
		return runtime, nil
	}
	command, err := exec.LookPath("usbip")
	if err != nil {
		return nil, fmt.Errorf("kata USB/IP support requires the usbip client: %w", err)
	}
	for _, path := range []string{USBIPDeviceRootPath, USBIPDevptsPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, fmt.Errorf("create USB/IP mount path %s: %w", path, err)
		}
	}
	// A worker can be restarted by the PID 1 supervisor after an unclean exit.
	// Remove only the mounts owned by this fixed Runtime path before rebuilding
	// them; this makes worker recovery independent of the previous process.
	if err := ignoreUnmountError(unix.Unmount(USBIPDevptsPath, unix.MNT_DETACH)); err != nil {
		return nil, fmt.Errorf("clean stale Dev Container devpts mount: %w", err)
	}
	if err := ignoreUnmountError(unix.Unmount(USBIPDeviceRootPath, unix.MNT_DETACH)); err != nil {
		return nil, fmt.Errorf("clean stale Kata Guest device mount: %w", err)
	}
	if err := unix.Mount("devtmpfs", USBIPDeviceRootPath, "devtmpfs", 0, ""); err != nil {
		return nil, fmt.Errorf("mount Kata Guest device tree: %w", err)
	}
	if err := unix.Mount("devpts", USBIPDevptsPath, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		_ = unix.Unmount(USBIPDeviceRootPath, unix.MNT_DETACH)
		return nil, fmt.Errorf("mount Dev Container devpts: %w", err)
	}
	runtime.enabled, runtime.command = true, command
	return runtime, nil
}

func (r *USBIPRuntime) Enabled() bool { return r != nil && r.enabled }

func (r *USBIPRuntime) Close() error {
	if r == nil {
		return nil
	}
	if !r.enabled {
		return nil
	}
	return errors.Join(
		ignoreUnmountError(unix.Unmount(USBIPDevptsPath, unix.MNT_DETACH)),
		ignoreUnmountError(unix.Unmount(USBIPDeviceRootPath, unix.MNT_DETACH)),
	)
}

func ignoreUnmountError(err error) error {
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func (r *USBIPRuntime) run(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, r.command, args...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if len(text) > 4096 {
			text = text[:4096]
		}
		if text == "" {
			text = err.Error()
		}
		return "", connect.NewError(connect.CodeFailedPrecondition, text).WithCause(err)
	}
	return text, nil
}
