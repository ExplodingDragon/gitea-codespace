// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"context"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

func TestInterruptedOperationWaitsForReconciliation(t *testing.T) {
	for _, command := range []string{"create", "resume", "stop", "delete"} {
		t.Run(command, func(t *testing.T) {
			operation := &codespacev1.OperationPayload{RuntimeUuid: "11111111-1111-4111-8111-111111111111", OperationRversion: 1}
			switch command {
			case "create":
				operation.Command = &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{}}
			case "resume":
				operation.Command = &codespacev1.OperationPayload_Resume{Resume: &codespacev1.ResumeOperationPayload{}}
			case "stop":
				operation.Command = &codespacev1.OperationPayload_Stop{Stop: &codespacev1.StopOperationPayload{}}
			case "delete":
				operation.Command = &codespacev1.OperationPayload_Delete{Delete: &codespacev1.DeleteOperationPayload{}}
			}
			agent := New(AgentConfig{InitialOperations: []OperationSnapshot{{Payload: operation, WorkerStage: OperationWorkerStageRecoveryBlocked}}}, nil, nil)
			if err := agent.startOperation(context.Background(), operation, time.Second); err != nil {
				t.Fatal(err)
			}
			if agent.activeOperations[operation.RuntimeUuid].running {
				t.Fatal("interrupted operation was replayed")
			}
			if len(agent.observedOperations()) != 0 {
				t.Fatal("interrupted operation would renew its lease indefinitely")
			}
			if err := agent.clearOperationContext(operation.RuntimeUuid, 1); err != nil {
				t.Fatal(err)
			}
			if len(agent.activeOperations) != 0 {
				t.Fatal("control-plane reconciliation did not clear the operation")
			}
		})
	}
}

func TestLeadershipLossCancelsDetachedCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agent := New(AgentConfig{ExecutionContext: ctx}, nil, nil)
	cancel()
	cleanup, stop := agent.newCleanupContext()
	defer stop()
	if cleanup.Err() == nil {
		t.Fatal("cleanup remained active after leadership loss")
	}
}
