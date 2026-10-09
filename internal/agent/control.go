// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/rpc/agent/v1/agentv1connect"
	"google.golang.org/protobuf/proto"
)

// ControlClient owns one execution at a time. Transport retries never retry an
// operation: its durable report and the execution journal determine recovery.
type ControlClient struct {
	Journal       *Journal
	Remote        agentv1connect.AgentControlServiceClient
	Execute       func(context.Context, *agentv1.ControlResponse, func(*agentv1.AgentReport), io.Writer, io.Writer) error
	Sample        func() *codespacev1.RuntimeResourceUsage
	TargetChanges <-chan struct{}
	CurrentTarget func() *agentv1.AccessTarget

	mu     sync.Mutex
	report *agentv1.AgentReport
}

func (c *ControlClient) snapshot() *agentv1.AgentReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proto.Clone(c.report).(*agentv1.AgentReport)
}

func (c *ControlClient) recordResult(executionErr error) error {
	c.mu.Lock()
	result := &agentv1.OperationResult{OperationRversion: c.report.OperationRversion, Status: codespacev1.FinalStatus_FINAL_STATUS_DONE}
	if executionErr != nil {
		result.Status = codespacev1.FinalStatus_FINAL_STATUS_FAILED
		// Detailed, redacted diagnostics belong in operation logs, not arbitrary
		// command errors that may contain credentials.
		result.Message = "Runtime execution failed; see the operation log"
	}
	c.report.Result = result
	encoded, err := proto.Marshal(c.report)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.Journal.write("state/report.pb", encoded)
}

func (c *ControlClient) startExecution(
	ctx context.Context,
	response *agentv1.ControlResponse,
	operation *codespacev1.OperationPayload,
	runtimeOptions *agentv1.RuntimeOptions,
	output *Output,
	wake chan<- struct{},
) (context.CancelFunc, chan error) {
	executionResponse := proto.Clone(response).(*agentv1.ControlResponse)
	executionResponse.Operation = proto.Clone(operation).(*codespacev1.OperationPayload)
	executionResponse.Runtime = proto.Clone(runtimeOptions).(*agentv1.RuntimeOptions)
	executionCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	var credentials []string
	if response.Access != nil {
		credentials = append(credentials, response.Access.GiteaToken)
		for _, secret := range response.Access.Secrets {
			credentials = append(credentials, secret.GetValue())
		}
	}
	output.replacements = NewOutput(output.log, credentials).replacements
	go func() {
		executionErr := c.Execute(executionCtx, executionResponse, func(update *agentv1.AgentReport) {
			c.mu.Lock()
			if update != nil && update.OperationRversion == c.report.OperationRversion {
				if update.Boot != nil {
					c.report.Boot = proto.Clone(update.Boot).(*codespacev1.RuntimeBoot)
				}
				if update.Target != nil {
					c.report.Target = proto.Clone(update.Target).(*agentv1.AccessTarget)
				}
				if update.ResourceUsage != nil {
					c.report.ResourceUsage = proto.Clone(update.ResourceUsage).(*codespacev1.RuntimeResourceUsage)
				}
			}
			c.mu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		}, output.Stdout(), output.Stderr())
		executionErr = errors.Join(executionErr, executionCtx.Err())
		if executionErr != nil {
			_, _ = fmt.Fprintf(output.Stderr(), "Error: %v\n", executionErr)
		}
		output.FlushLines()
		if err := output.Err(); err != nil {
			slog.Warn("Persist Agent operation output", "error", err)
		}
		// Output transport is independent of the execution outcome.
		drain, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = output.log.Flush(drain, c.Remote)
		cancelDrain()
		done <- executionErr
		select {
		case wake <- struct{}{}:
		default:
		}
	}()
	return cancel, done
}

// Run retains completed facts until Manager has moved to another operation. A
// lost result acknowledgement therefore cannot replay a completed create.
func (c *ControlClient) Run(ctx context.Context) (returnErr error) {
	if c.Journal == nil || c.Remote == nil || c.Execute == nil {
		return fmt.Errorf("agent control dependencies are incomplete")
	}
	c.report = &agentv1.AgentReport{}
	data, err := c.Journal.read("state/report.pb", 1024*1024)
	if err == nil {
		if err := proto.Unmarshal(data, c.report); err != nil {
			return fmt.Errorf("read saved Agent report: %w", err)
		}
		if c.report.OperationRversion <= 0 || (c.report.Result != nil && c.report.Result.OperationRversion != c.report.OperationRversion) {
			return fmt.Errorf("saved Agent report has an invalid operation identity")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var permit Permit
	var acceptedOperation *codespacev1.OperationPayload
	var runtimeOptions *agentv1.RuntimeOptions
	var cancelExecution context.CancelFunc
	var finished chan error
	var executionTimer *time.Timer
	var lastStarted int64
	var log *OperationLog
	var logOffset int64
	var output *Output
	var stopUpload context.CancelFunc
	var uploadDone chan struct{}
	wake := make(chan struct{}, 1)
	metrics := time.NewTicker(10 * time.Second)
	defer metrics.Stop()
	refreshMetrics := true
	defer func() {
		if executionTimer != nil {
			executionTimer.Stop()
		}
		if cancelExecution != nil {
			cancelExecution()
			returnErr = errors.Join(returnErr, c.recordResult(<-finished))
		}
		if stopUpload != nil {
			stopUpload()
			<-uploadDone
			returnErr = errors.Join(returnErr, log.Close())
		}
	}()
	for ctx.Err() == nil {
		permit.Reconnect()
		sessionCtx, cancelSession := context.WithCancel(ctx)
		stream := c.Remote.Control(sessionCtx)
		for sessionCtx.Err() == nil {
			if refreshMetrics && c.Sample != nil {
				c.mu.Lock()
				c.report.ResourceUsage = c.Sample()
				c.mu.Unlock()
				refreshMetrics = false
			}
			if finished != nil {
				select {
				case executionErr := <-finished:
					cancelExecution()
					if executionTimer != nil {
						executionTimer.Stop()
						executionTimer = nil
					}
					cancelExecution, finished = nil, nil
					if err := c.recordResult(executionErr); err != nil {
						cancelSession()
						return err
					}
				default:
				}
			}
			report := c.snapshot()
			if report.OperationRversion > 0 && (log == nil || log.version != report.OperationRversion) {
				if stopUpload != nil {
					stopUpload()
					<-uploadDone
					_ = log.Close()
					stopUpload = nil
				}
				if err := c.Journal.PruneLogs(report.OperationRversion); err != nil {
					cancelSession()
					return err
				}
				// Reopen saved batches even when only a completed result remains.
				// Their checkpoint, not the current Gitea offset, controls replay.
				log, err = OpenOperationLog(c.Journal, report.OperationRversion, logOffset)
				if err != nil {
					cancelSession()
					return err
				}
				uploadCtx, cancel := context.WithCancel(ctx)
				stopUpload, uploadDone = cancel, make(chan struct{})
				output = NewOutput(log, nil)
				pending, done := output, uploadDone
				go func() {
					defer close(done)
					pending.Upload(uploadCtx, c.Remote)
				}()
			}
			session, sequence, err := permit.Request(report.OperationRversion, time.Now())
			if err != nil {
				cancelSession()
				return err
			}
			// Execution has its own deadline timer, independent of a blocked
			// transport call or the next periodic control report.
			requestTimeout := time.AfterFunc(10*time.Second, cancelSession)
			acceptedVersion := int64(0)
			if acceptedOperation != nil {
				acceptedVersion = acceptedOperation.OperationRversion
			}
			err = stream.Send(&agentv1.ControlRequest{ProtocolVersion: 1, SessionId: session, Sequence: sequence, Report: report, AcceptedOperationRversion: acceptedVersion})
			var response *agentv1.ControlResponse
			if err == nil {
				response, err = stream.Receive()
			}
			requestTimeout.Stop()
			if err != nil || sessionCtx.Err() != nil {
				break
			}
			if response.SessionId != session || response.Sequence != sequence {
				break
			}
			if response.Operation != nil {
				acceptedOperation = proto.Clone(response.Operation).(*codespacev1.OperationPayload)
				if response.Runtime == nil {
					cancelSession()
					return fmt.Errorf("manager omitted Runtime options for a new operation")
				}
				runtimeOptions = proto.Clone(response.Runtime).(*agentv1.RuntimeOptions)
			}
			operation := acceptedOperation
			if operation != nil {
				if operation.RuntimeUuid != c.Journal.record.Identity.RuntimeUUID || operation.OperationRversion <= 0 || operation.OperationRversion < report.OperationRversion || operation.LogOffset < 0 {
					cancelSession()
					return fmt.Errorf("manager operation does not match the runtime identity or history")
				}
				if operation.OperationRversion != report.OperationRversion {
					if cancelExecution != nil {
						cancelExecution()
					} else {
						logOffset = operation.LogOffset
						c.mu.Lock()
						c.report = &agentv1.AgentReport{OperationRversion: operation.OperationRversion}
						c.mu.Unlock()
					}
					// The next session reports the new version before requesting its
					// access bundle. An old operation's key/report cannot accept it.
					break
				}
				if finished == nil && runtimeOptions != nil && len(c.snapshot().GitSshPublicKey) == 0 && (operation.GetCreate() != nil || operation.GetResume() != nil) {
					key, keyErr := c.Journal.GitSSHKey(runtimeOptions.GitSshKeyType, operation.GetCreate() != nil)
					if keyErr != nil {
						cancelSession()
						return keyErr
					}
					c.mu.Lock()
					c.report.GitSshPublicKey = key.PublicKey().Marshal()
					c.mu.Unlock()
				}
			}
			deadline, permitErr := permit.Accept(response, time.Now())
			if permitErr != nil {
				if cancelExecution != nil {
					cancelExecution()
				}
			} else if operation == nil || runtimeOptions == nil {
				cancelSession()
				return fmt.Errorf("manager did not provide the current operation")
			} else if finished == nil && operation.OperationRversion != lastStarted && c.snapshot().Result == nil && ((operation.GetCreate() == nil && operation.GetResume() == nil) || response.Access != nil) {
				lastStarted = operation.OperationRversion
				cancelExecution, finished = c.startExecution(ctx, response, operation, runtimeOptions, output, wake)
			}
			if permitErr == nil && cancelExecution != nil {
				if executionTimer == nil {
					cancel, version := cancelExecution, operation.OperationRversion
					executionTimer = time.AfterFunc(time.Until(deadline), func() {
						if !permit.Valid(version, time.Now()) {
							cancel()
						}
					})
				} else {
					executionTimer.Reset(time.Until(deadline))
				}
			}
			wait := 5 * time.Second
			if permitErr == nil {
				remaining := time.Until(deadline)
				if renewal := remaining / 3; renewal > 0 && renewal < wait {
					wait = renewal
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-sessionCtx.Done():
			case <-timer.C:
			case <-wake:
			case <-metrics.C:
				refreshMetrics = true
			case <-c.TargetChanges:
				if c.CurrentTarget != nil {
					target := c.CurrentTarget()
					c.mu.Lock()
					if target != nil && c.report.Target != nil && target.Version > c.report.Target.Version {
						c.report.Target = target
					}
					c.mu.Unlock()
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		cancelSession()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}
