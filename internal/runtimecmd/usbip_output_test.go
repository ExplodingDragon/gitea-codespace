// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"encoding/json"
	"testing"

	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestUSBIPDocument(t *testing.T) {
	list, err := USBIPDocument(agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST, &agentv1.RuntimeUSBIPServiceRunResponse{Sources: []*agentv1.USBIPSource{{Name: "workstation", Port: 43001, Output: `Exportable USB devices
======================
 - 127.0.0.1
        1-1: Linux Foundation : CDC ACM (1d6b:0104)
           : /sys/devices/virtual/usb
           : Miscellaneous Device (ef/02/01)
           :  0 - Communications (02/02/01)
`}, {Name: "offline", Error: "connection refused"}}})
	require.NoError(t, err)
	require.Len(t, list.Devices, 1)
	require.Equal(t, "1-1", list.Devices[0].BusID)
	require.Equal(t, "1d6b", list.Devices[0].VendorID)
	require.Equal(t, "workstation", list.Devices[0].Source)
	require.Equal(t, "connection refused", list.Sources[1].Error)

	status, err := USBIPDocument(agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, &agentv1.RuntimeUSBIPServiceRunResponse{Sources: []*agentv1.USBIPSource{{Name: "workstation", Port: 43001}}, Output: `Imported USB devices
====================
Port 00: <Port in Use> at High Speed(480Mbps)
       Linux Foundation : CDC ACM (1d6b:0104)
       2-1 -> usbip://127.0.0.1:43001/1-1
           -> remote bus/dev 001/001
Port 01: <Port Error> at Full Speed(12Mbps)
       unknown product (abcd:1234)
       2-2 -> unknown host, remote port and remote busid
           -> remote bus/dev 002/002
`})
	require.NoError(t, err)
	require.Len(t, status.Devices, 2)
	require.Equal(t, "1-1", status.Devices[0].BusID)
	require.Equal(t, uint16(0), *status.Devices[0].Port)
	require.Equal(t, "Port Error", status.Devices[1].State)
	require.Empty(t, status.Devices[1].BusID)
	encoded, err := json.Marshal(status)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"port":0`)
	require.Equal(t, "workstation", status.Devices[0].Source)
}

func TestUSBIPDocumentEmptyAndMalformed(t *testing.T) {
	for _, test := range []struct {
		operation agentv1.RuntimeUSBIPOperation
		output    string
	}{
		{agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_LIST, "usbip: info: no exportable devices found on 127.0.0.1"},
		{agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, "Imported USB devices\n====================\n"},
	} {
		document, err := USBIPDocument(test.operation, &agentv1.RuntimeUSBIPServiceRunResponse{Output: test.output, Sources: []*agentv1.USBIPSource{{Name: "workstation", Output: test.output}}})
		require.NoError(t, err)
		require.Empty(t, document.Devices)
	}
	for _, output := range []string{"", "unrecognized response", "Imported USB devices\nPort changed format", "Imported USB devices\nPort 00: <Port in Use> at High Speed(480Mbps)"} {
		_, err := USBIPDocument(agentv1.RuntimeUSBIPOperation_RUNTIME_USBIP_OPERATION_STATUS, &agentv1.RuntimeUSBIPServiceRunResponse{Output: output})
		require.Error(t, err)
	}
}
