// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"golang.org/x/sys/unix"
)

const DockerHost = "unix:///run/codespace/docker.sock"

// Supervise is the PID 1 branch of the Agent executable. The worker owns its
// normal os/exec children; only orphans reach this parent. A single wait4 loop
// therefore reaps orphaned processes without stealing os/exec exit statuses.
func Supervise(ctx context.Context, executable string, args []string, stdin, stdout, stderr *os.File) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("agent supervisor must run as container PID 1")
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD)
	defer signal.Stop(signals)
	child, err := os.StartProcess(executable, append([]string{executable}, args...), &os.ProcAttr{
		Env: os.Environ(), Files: []*os.File{stdin, stdout, stderr}, Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return err
	}
	defer func() { _ = child.Release() }()
	var deadline <-chan time.Time
	ctxDone := ctx.Done()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var status unix.WaitStatus
		for {
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil && !errors.Is(err, unix.ECHILD) {
				return fmt.Errorf("reap Agent child: %w", err)
			}
			if pid <= 0 {
				break
			}
			if pid == child.Pid {
				if status.Exited() && status.ExitStatus() == 0 {
					return nil
				}
				return fmt.Errorf("agent worker exited: %v", status)
			}
		}
		select {
		case event := <-signals:
			if event == syscall.SIGCHLD {
				continue
			}
			// Signal the worker, not all of Docker's children: the worker must
			// first finish the active operation and then shut down the daemon.
			_ = child.Signal(event)
		case <-ctxDone:
			ctxDone = nil
			_ = child.Signal(syscall.SIGTERM)
		case <-deadline:
			_ = unix.Kill(-child.Pid, unix.SIGKILL)
			deadline = nil
		}
		if timer == nil {
			timer = time.NewTimer(50 * time.Second)
			deadline = timer.C
		}
	}
}

// DockerDaemon belongs to one volume writer and is stopped before releasing the
// journal lock. Its persistent data and volatile sockets have separate roots.
type DockerDaemon struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func StartDocker(ctx context.Context, directory string, insecureRegistries []string, output io.Writer) (*DockerDaemon, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime network interfaces: %w", err)
	}
	mtu := runtimeNetworkMTU(interfaces)
	if mtu == 0 {
		return nil, fmt.Errorf("runtime has no usable network interface MTU")
	}
	arguments := []string{"--host=" + DockerHost, "--data-root=" + directory + "/docker",
		"--exec-root=/run/codespace/docker", "--pidfile=/run/codespace/docker.pid",
		"--feature=containerd-snapshotter=true", "--live-restore=false", "--shutdown-timeout=30", fmt.Sprintf("--mtu=%d", mtu)}
	seen := make(map[string]struct{}, len(insecureRegistries))
	for _, value := range insecureRegistries {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("invalid internal cache registry %q", value)
		}
		if _, ok := seen[parsed.Host]; ok {
			continue
		}
		seen[parsed.Host] = struct{}{}
		arguments = append(arguments, "--insecure-registry="+parsed.Host)
	}
	command := exec.Command("dockerd", arguments...)
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Docker daemon: %w", err)
	}
	daemon := &DockerDaemon{command: command, done: make(chan struct{})}
	go func() { daemon.err = command.Wait(); close(daemon.done) }()
	docker, err := client.NewClientWithOpts(client.WithHost(DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		_ = daemon.Close()
		return nil, err
	}
	defer func() { _ = docker.Close() }()
	readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		pingCtx, pingCancel := context.WithTimeout(readyCtx, time.Second)
		_, err := docker.Ping(pingCtx)
		pingCancel()
		if err == nil {
			return daemon, nil
		}
		select {
		case <-daemon.done:
			return nil, fmt.Errorf("docker exited before becoming ready: %w", daemon.err)
		case <-readyCtx.Done():
			_ = daemon.Close()
			return nil, fmt.Errorf("wait for Docker: %w", readyCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func runtimeNetworkMTU(interfaces []net.Interface) int {
	mtu := 0
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagLoopback != 0 || networkInterface.Flags&net.FlagUp == 0 || networkInterface.MTU < 576 {
			continue
		}
		if mtu == 0 || networkInterface.MTU < mtu {
			mtu = networkInterface.MTU
		}
	}
	return mtu
}

func (d *DockerDaemon) Done() <-chan struct{} { return d.done }

func (d *DockerDaemon) Close() error {
	select {
	case <-d.done:
		return d.err
	default:
	}
	_ = d.command.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	select {
	case <-d.done:
		return d.err
	case <-timer.C:
		_ = unix.Kill(-d.command.Process.Pid, unix.SIGKILL)
		<-d.done
		return fmt.Errorf("docker did not stop within its shutdown deadline")
	}
}
