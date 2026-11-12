// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
)

type usbIPDevice struct {
	Source      string  `json:"source"`
	BusID       string  `json:"busId"`
	Description string  `json:"description"`
	VendorID    string  `json:"vendorId,omitempty"`
	ProductID   string  `json:"productId,omitempty"`
	Port        *uint16 `json:"port,omitempty"`
	State       string  `json:"state,omitempty"`
}

type usbIPDocument struct {
	Version int           `json:"version"`
	Sources []usbIPSource `json:"sources"`
	Devices []usbIPDevice `json:"devices"`
}

type usbIPSource struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

var (
	usbIPRemoteDevice = regexp.MustCompile(`^([0-9]+-[0-9]+(?:\.[0-9]+)*): (.+)$`)
	usbIPImportedPort = regexp.MustCompile(`^Port ([0-9]+): <([^>]+)> at .+$`)
	usbIPRemoteBusID  = regexp.MustCompile(` -> (usbip://[^/]+/[0-9]+-[0-9]+(?:\.[0-9]+)*)$`)
	usbIPProductIDs   = regexp.MustCompile(`\(([[:xdigit:]]{4}):([[:xdigit:]]{4})\)$`)
)

// USBIPDocument adapts the platform-owned Linux usbip client output once, so
// plugins consume a stable document instead of each parsing terminal text.
func USBIPDocument(operation agentv1.RuntimeUSBIPOperation, response *agentv1.RuntimeUSBIPServiceRunResponse) (*usbIPDocument, error) {
	document := &usbIPDocument{Version: 1, Sources: []usbIPSource{}, Devices: []usbIPDevice{}}
	listing := operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST
	for _, source := range response.Sources {
		entry := usbIPSource{Name: source.Name, Error: source.Error}
		if listing && entry.Error == "" {
			devices, err := parseUSBIPDevices(source.Output, true, response.Sources)
			if err != nil {
				entry.Error = err.Error()
			} else {
				for i := range devices {
					devices[i].Source = source.Name
				}
				document.Devices = append(document.Devices, devices...)
			}
		}
		document.Sources = append(document.Sources, entry)
	}
	if listing || operation == agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_SOURCES {
		return document, nil
	}
	if operation != agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS {
		return nil, fmt.Errorf("USB/IP JSON output requires sources, list or status")
	}
	devices, err := parseUSBIPDevices(response.Output, false, response.Sources)
	document.Devices = devices
	return document, err
}

func parseUSBIPDevices(output string, listing bool, sources []*agentv1.USBIPSource) ([]usbIPDevice, error) {
	devices := []usbIPDevice{}
	header := "Imported USB devices"
	if listing {
		header = "Exportable USB devices"
	}
	seenHeader := false
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.Trim(line, "=") == "" || strings.HasPrefix(line, "usbip: error: failed to open ") || strings.HasPrefix(line, "libusbip: error: ") || strings.HasPrefix(line, "usbip: info: using port ") {
			// usb.ids and an imported port's saved remote record are optional.
			continue
		}
		if line == header || (listing && line == "usbip: info: no exportable devices found on 127.0.0.1") {
			seenHeader = true
			continue
		}
		if !seenHeader {
			return nil, fmt.Errorf("unrecognized USB/IP output: %s", line)
		}
		if listing {
			if line == "- 127.0.0.1" || strings.HasPrefix(line, ":") {
				continue
			}
			match := usbIPRemoteDevice.FindStringSubmatch(line)
			if match == nil {
				return nil, fmt.Errorf("invalid USB/IP device: %s", line)
			}
			devices = append(devices, usbIPDevice{BusID: match[1], Description: match[2]})
		} else {
			if match := usbIPImportedPort.FindStringSubmatch(line); match != nil {
				port, err := strconv.ParseUint(match[1], 10, 16)
				if err != nil {
					return nil, fmt.Errorf("invalid USB/IP port: %w", err)
				}
				devices = append(devices, usbIPDevice{Port: new(uint16(port)), State: match[2]})
				continue
			}
			if len(devices) == 0 {
				return nil, fmt.Errorf("invalid USB/IP status: %s", line)
			}
			device := &devices[len(devices)-1]
			switch {
			case device.Description == "":
				device.Description = line
			case usbIPRemoteBusID.MatchString(line):
				remote, err := url.Parse(usbIPRemoteBusID.FindStringSubmatch(line)[1])
				if err != nil {
					return nil, fmt.Errorf("invalid USB/IP remote address: %w", err)
				}
				device.BusID = strings.TrimPrefix(remote.Path, "/")
				for _, source := range sources {
					if remote.Hostname() == "127.0.0.1" && remote.Port() == strconv.FormatUint(uint64(source.Port), 10) {
						device.Source = source.Name
						break
					}
				}
			case strings.HasPrefix(line, "-> remote bus/dev "), strings.HasSuffix(line, "-> unknown host, remote port and remote busid"):
			default:
				return nil, fmt.Errorf("invalid USB/IP device status: %s", line)
			}
		}
	}
	if !seenHeader {
		return nil, fmt.Errorf("missing USB/IP output header")
	}
	for i := range devices {
		device := &devices[i]
		if device.Description == "" {
			return nil, fmt.Errorf("incomplete USB/IP device description")
		}
		if ids := usbIPProductIDs.FindStringSubmatch(device.Description); ids != nil {
			device.VendorID, device.ProductID = strings.ToLower(ids[1]), strings.ToLower(ids[2])
		}
	}
	return devices, nil
}
