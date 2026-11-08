// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

func TestEndpointCommandsMapArguments(t *testing.T) {
	var gotLabel string
	var gotPort uint16
	var gotPublic bool
	list := func(context.Context) ([]*codespacev1.RuntimeEndpoint, error) { return nil, nil }
	command := newEndpointCommandWithRun(list, func(_ context.Context, port uint16, label string, public bool) error {
		gotLabel, gotPort, gotPublic = label, port, public
		return nil
	}, func(context.Context, uint16) error {
		t.Fatal("delete called while testing set")
		return nil
	})
	command.SetArgs([]string{"set", "8443", "--label", "Web preview", "--public"})
	if err := command.Execute(); err != nil {
		t.Fatalf("set Execute() error = %v", err)
	}
	if gotLabel != "Web preview" || gotPort != 8443 || !gotPublic {
		t.Fatalf("set arguments = (%q, %d, %t)", gotLabel, gotPort, gotPublic)
	}

	var deletedPort uint16
	command = newEndpointCommandWithRun(list, func(context.Context, uint16, string, bool) error {
		t.Fatal("set called while testing delete")
		return nil
	}, func(_ context.Context, port uint16) error {
		deletedPort = port
		return nil
	})
	command.SetArgs([]string{"delete", "8443"})
	if err := command.Execute(); err != nil {
		t.Fatalf("delete Execute() error = %v", err)
	}
	if deletedPort != 8443 {
		t.Fatalf("deleted endpoint port = %d", deletedPort)
	}
}

func TestEndpointListJSON(t *testing.T) {
	list := func(context.Context) ([]*codespacev1.RuntimeEndpoint, error) {
		return []*codespacev1.RuntimeEndpoint{
			{Port: 3000, Label: "Application", Public: true},
			{Port: 8080, Label: "Documentation"},
		}, nil
	}
	command := newEndpointCommandWithRun(list, nil, nil)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"list", "--format", "json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("list Execute() error = %v", err)
	}
	var result struct {
		Version   int `json:"version"`
		Endpoints []struct {
			Port       uint32 `json:"port"`
			Label      string `json:"label"`
			Visibility string `json:"visibility"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Version != 1 || len(result.Endpoints) != 2 || result.Endpoints[0].Visibility != "public" || result.Endpoints[1].Visibility != "authenticated" {
		t.Fatalf("JSON output = %#v", result)
	}
}
