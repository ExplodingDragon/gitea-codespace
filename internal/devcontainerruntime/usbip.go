// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	volumemounts "github.com/docker/docker/volume/mounts"
)

const defaultContainerShmSize = 64 * 1024 * 1024

func usbIPContainerMutator(configuration USBIPContainerConfiguration) containerdocker.MutateContainerCreateFunc {
	if !configuration.Enabled {
		return nil
	}
	return func(config *container.Config, host *container.HostConfig) error {
		targets := map[string]struct{}{}
		for _, item := range host.Mounts {
			targets[path.Clean(item.Target)] = struct{}{}
		}
		parser := volumemounts.NewParser()
		for _, item := range host.Binds {
			parsed, err := parser.ParseMountRaw(item, host.VolumeDriver)
			if err != nil {
				return fmt.Errorf("parse Dev Container bind mount: %w", err)
			}
			targets[path.Clean(parsed.Destination)] = struct{}{}
		}
		for target := range host.Tmpfs {
			targets[path.Clean(target)] = struct{}{}
		}
		for target := range config.Volumes {
			targets[path.Clean(target)] = struct{}{}
		}
		for _, target := range []string{"/dev", "/dev/pts"} {
			if _, exists := targets[target]; exists {
				return fmt.Errorf("kata USB/IP requires platform ownership of %s", target)
			}
		}
		devices, err := os.Open("/proc/devices")
		if err != nil {
			return fmt.Errorf("read Kata guest device classes: %w", err)
		}
		defer devices.Close()
		rules, err := usbIPDeviceCgroupRules(devices)
		if err != nil {
			return fmt.Errorf("read Kata guest device classes: %w", err)
		}
		host.Mounts = append(host.Mounts,
			mount.Mount{Type: mount.TypeBind, Source: configuration.DeviceRoot, Target: "/dev", BindOptions: &mount.BindOptions{NonRecursive: true}},
			mount.Mount{Type: mount.TypeBind, Source: configuration.DevptsRoot, Target: "/dev/pts", BindOptions: &mount.BindOptions{NonRecursive: true}},
		)
		if _, exists := targets["/dev/shm"]; !exists && (host.IpcMode.IsEmpty() || host.IpcMode.IsPrivate()) {
			size := host.ShmSize
			if size == 0 {
				size = defaultContainerShmSize
			}
			host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeTmpfs, Target: "/dev/shm", TmpfsOptions: &mount.TmpfsOptions{SizeBytes: size, Mode: 0o1777}})
		}
		// eudev needs netlink send permission for worker notifications and
		// processed device events, beyond receiving kernel uevents.
		host.CapAdd = appendUniqueStrings(host.CapAdd, "NET_ADMIN")
		host.DeviceCgroupRules = appendUniqueStrings(host.DeviceCgroupRules, rules...)
		return nil
	}
}

func usbIPDeviceCgroupRules(devices io.Reader) ([]string, error) {
	// USB bus, USB serial, ACM, I2C, SPI and USB miscellaneous devices use
	// fixed majors. HIDRAW and GPIO majors are allocated by the guest kernel.
	rules := []string{"c 189:* rwm", "c 188:* rwm", "c 166:* rwm", "c 89:* rwm", "c 153:* rwm", "c 180:* rwm"}
	characters := false
	scanner := bufio.NewScanner(devices)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "Character devices:":
			characters = true
			continue
		case "Block devices:":
			characters = false
			continue
		}
		fields := strings.Fields(line)
		if !characters || len(fields) != 2 || (fields[1] != "hidraw" && fields[1] != "gpiochip") {
			continue
		}
		major, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse %s major: %w", fields[1], err)
		}
		rules = append(rules, fmt.Sprintf("c %d:* rwm", major))
	}
	return rules, scanner.Err()
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, value := range additions {
		if !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	return values
}

func prepareUSBIPContainer(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, configuration USBIPContainerConfiguration) error {
	if !configuration.Enabled {
		return nil
	}
	pid, ipcMode, err := engine.ContainerProcess(ctx, state.PrimaryContainerID)
	if err != nil {
		return err
	}
	if ipcMode == "none" {
		return nil
	}
	root := "/proc/" + strconv.Itoa(pid) + "/root"
	arguments := []string{"--target", strconv.Itoa(pid), "--mount", "--ipc", "--wd=" + root}
	run := func(command ...string) error {
		process := exec.CommandContext(ctx, "nsenter", append(arguments, command...)...)
		output, err := process.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", command[0], err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	if err := run("mkdir", "-p", "dev/mqueue"); err != nil {
		return fmt.Errorf("prepare Dev Container message queue path: %w", err)
	}
	check := exec.CommandContext(ctx, "nsenter", append(arguments, "mountpoint", "-q", "dev/mqueue")...)
	if err := check.Run(); err == nil {
		return nil
	} else if exit := (&exec.ExitError{}); !errors.As(err, &exit) || (exit.ExitCode() != 1 && exit.ExitCode() != 32) {
		return fmt.Errorf("inspect Dev Container message queue mount: %w", err)
	}
	if err := run("mount", "--no-canonicalize", "-t", "mqueue", "-o", "nosuid,nodev,noexec", "mqueue", "dev/mqueue"); err != nil {
		return fmt.Errorf("mount Dev Container message queue: %w", err)
	}
	return nil
}
