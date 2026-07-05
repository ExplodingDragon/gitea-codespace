// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrNotFound indicates that a record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized indicates that a token or caller is invalid.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrConflict indicates that the requested mutation conflicts with current state.
	ErrConflict = errors.New("conflict")
)

// RegistrationToken stores a manager bootstrap token.
type RegistrationToken struct {
	Name       string     `json:"name"`
	Token      string     `json:"token,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	IsActive   bool       `json:"is_active"`
}

// Manager stores manager state reported to the control plane.
type Manager struct {
	ID                 int64                            `json:"id"`
	UUID               string                           `json:"uuid"`
	Name               string                           `json:"name"`
	GatewayURL         string                           `json:"gateway_url"`
	Version            string                           `json:"version"`
	Status             codespacev1.ManagerStatus        `json:"status"`
	MaxConcurrency     int32                            `json:"max_concurrency"`
	CurrentConcurrency int32                            `json:"current_concurrency"`
	Load               float64                          `json:"load"`
	LastOnlineAt       time.Time                        `json:"last_online_at"`
	Token              string                           `json:"-"`
	Capabilities       *codespacev1.ManagerCapabilities `json:"capabilities,omitempty"`
}

// RuntimeContext stores runtime API context for a codespace.
type RuntimeContext struct {
	Token       string    `json:"token,omitempty"`
	CodespaceID string    `json:"codespace_id"`
	Repo        string    `json:"repo"`
	Ref         string    `json:"ref"`
	Root        string    `json:"root"`
	Phase       string    `json:"phase"`
	Message     string    `json:"message"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Port stores one preview port exposed by a codespace.
type Port struct {
	Name        string                     `json:"name"`
	Port        int32                      `json:"port"`
	Protocol    string                     `json:"protocol"`
	Visibility  codespacev1.PortVisibility `json:"visibility"`
	Description string                     `json:"description"`
	PublicURL   string                     `json:"public_url"`
	Status      codespacev1.PortStatus     `json:"status"`
}

// Codespace stores one codespace lifecycle record.
type Codespace struct {
	ID                string                      `json:"id"`
	Owner             string                      `json:"owner"`
	RepoName          string                      `json:"repo_name"`
	RepoFullName      string                      `json:"repo_full_name"`
	RepoURL           string                      `json:"repo_url"`
	UserID            int64                       `json:"user_id"`
	RepoID            int64                       `json:"repo_id"`
	RefType           string                      `json:"ref_type"`
	RefName           string                      `json:"ref_name"`
	CommitSHA         string                      `json:"commit_sha"`
	PullID            int64                       `json:"pull_id"`
	TargetBranch      string                      `json:"target_branch"`
	HeadBranch        string                      `json:"head_branch"`
	HeadRepo          string                      `json:"head_repo"`
	ManagerID         int64                       `json:"manager_id"`
	ManagerUUID       string                      `json:"manager_uuid"`
	ActiveOperationID string                      `json:"active_operation_id"`
	InstanceName      string                      `json:"instance_name"`
	InstanceType      string                      `json:"instance_type"`
	Image             string                      `json:"image"`
	ResourcePreset    string                      `json:"resource_preset"`
	Status            codespacev1.CodespaceStatus `json:"status"`
	GatewayURL        string                      `json:"gateway_url"`
	Workdir           string                      `json:"workdir"`
	LastActiveAt      time.Time                   `json:"last_active_at"`
	CreatedAt         time.Time                   `json:"created_at"`
	UpdatedAt         time.Time                   `json:"updated_at"`
	StoppedAt         *time.Time                  `json:"stopped_at,omitempty"`
	DeletedAt         *time.Time                  `json:"deleted_at,omitempty"`
	ErrorMessage      string                      `json:"error_message,omitempty"`
	Runtime           *RuntimeContext             `json:"runtime,omitempty"`
	Ports             map[string]*Port            `json:"ports,omitempty"`
	GitToken          string                      `json:"-"`
	GitTokenUsername  string                      `json:"git_token_username,omitempty"`
	GitTokenExpireAt  *time.Time                  `json:"git_token_expires_at,omitempty"`
}

// Task stores one manager task.
type Task struct {
	ID            string                                 `json:"id"`
	OperationID   string                                 `json:"operation_id"`
	CodespaceID   string                                 `json:"codespace_id"`
	ManagerUUID   string                                 `json:"manager_uuid"`
	Type          codespacev1.OperationType              `json:"type"`
	Status        codespacev1.OperationStatus            `json:"status"`
	Priority      int                                    `json:"priority"`
	Payload       *codespacev1.CodespaceOperationPayload `json:"payload,omitempty"`
	Result        *codespacev1.TaskResult                `json:"result,omitempty"`
	LeaseDeadline *time.Time                             `json:"lease_deadline,omitempty"`
	ErrorMessage  string                                 `json:"error_message,omitempty"`
	Attempts      int                                    `json:"attempts"`
	CreatedAt     time.Time                              `json:"created_at"`
	UpdatedAt     time.Time                              `json:"updated_at"`
}

// LogEntry stores one operation log line.
type LogEntry struct {
	ID          int64     `json:"id"`
	CodespaceID string    `json:"codespace_id"`
	OperationID string    `json:"operation_id"`
	TaskID      string    `json:"task_id"`
	ManagerID   int64     `json:"manager_id"`
	Level       string    `json:"level"`
	Message     string    `json:"message"`
	Sequence    int64     `json:"sequence"`
	CreatedAt   time.Time `json:"created_at"`
}

// AccessTicket stores one open-codespace ticket.
type AccessTicket struct {
	Token       string    `json:"token,omitempty"`
	UserID      int64     `json:"user_id"`
	CodespaceID string    `json:"codespace_id"`
	RepoID      int64     `json:"repo_id"`
	Action      string    `json:"action"`
	ExpiresAt   time.Time `json:"expires_at"`
	Consumed    bool      `json:"consumed"`
}

// Session stores one gateway session.
type Session struct {
	Token       string    `json:"token,omitempty"`
	UserID      int64     `json:"user_id"`
	CodespaceID string    `json:"codespace_id"`
	RepoID      int64     `json:"repo_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// CreateCodespaceInput describes one codespace creation request.
type CreateCodespaceInput struct {
	Owner          string `json:"owner"`
	RepoName       string `json:"repo_name"`
	RepoURL        string `json:"repo_url"`
	UserID         int64  `json:"user_id"`
	RepoID         int64  `json:"repo_id"`
	RefType        string `json:"ref_type"`
	RefName        string `json:"ref_name"`
	CommitSHA      string `json:"commit_sha"`
	PullID         int64  `json:"pull_id"`
	TargetBranch   string `json:"target_branch"`
	HeadBranch     string `json:"head_branch"`
	HeadRepo       string `json:"head_repo"`
	ManagerID      int64  `json:"manager_id"`
	InstanceType   string `json:"instance_type"`
	Image          string `json:"image"`
	ResourcePreset string `json:"resource_preset"`
	InitScript     string `json:"init_script"`
}

// MemoryStore stores all runtime state in memory.
type MemoryStore struct {
	mu                 sync.Mutex
	nextManagerID      int64
	registrationTokens map[string]*RegistrationToken
	managers           map[string]*Manager
	managersByID       map[int64]*Manager
	codespace          map[string]*Codespace
	tasks              map[string]*Task
	logs               map[string][]*LogEntry
	nextLogID          int64
	accessTickets      map[string]*AccessTicket
	sessions           map[string]*Session
	runtimeByToken     map[string]*RuntimeContext
}

// New creates one in-memory store.
func New() *MemoryStore {
	return &MemoryStore{
		nextManagerID:      1,
		registrationTokens: make(map[string]*RegistrationToken),
		managers:           make(map[string]*Manager),
		managersByID:       make(map[int64]*Manager),
		codespace:          make(map[string]*Codespace),
		tasks:              make(map[string]*Task),
		logs:               make(map[string][]*LogEntry),
		nextLogID:          1,
		accessTickets:      make(map[string]*AccessTicket),
		sessions:           make(map[string]*Session),
		runtimeByToken:     make(map[string]*RuntimeContext),
	}
}

// EnsureRegistrationToken creates or refreshes one bootstrap token.
func (s *MemoryStore) EnsureRegistrationToken(token string, name string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if token == "" {
		return fmt.Errorf("token is empty: %w", ErrConflict)
	}

	var expiresAt *time.Time
	if ttl > 0 {
		value := time.Now().Add(ttl)
		expiresAt = &value
	}

	s.registrationTokens[token] = &RegistrationToken{
		Name:      name,
		Token:     token,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
		IsActive:  true,
	}
	return nil
}

// ResetRegistrationToken creates one active registration token and invalidates old tokens.
func (s *MemoryStore) ResetRegistrationToken(ttl time.Duration) (*RegistrationToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := randomToken("reg")
	if err != nil {
		return nil, fmt.Errorf("random token: %w", err)
	}

	for _, oldToken := range s.registrationTokens {
		oldToken.IsActive = false
	}

	var expiresAt *time.Time
	if ttl > 0 {
		value := time.Now().Add(ttl)
		expiresAt = &value
	}

	value := &RegistrationToken{
		Name:      "codespace-manager",
		Token:     token,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
		IsActive:  true,
	}
	s.registrationTokens[token] = value

	return cloneRegistrationToken(value), nil
}

// GetRegistrationToken returns the newest active registration token.
func (s *MemoryStore) GetRegistrationToken() (*RegistrationToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var latest *RegistrationToken
	for _, token := range s.registrationTokens {
		if !token.IsActive {
			continue
		}
		if latest == nil || latest.CreatedAt.Before(token.CreatedAt) {
			latest = token
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("registration token: %w", ErrNotFound)
	}
	return cloneRegistrationToken(latest), nil
}

// RegisterManager registers one manager and returns its session token.
func (s *MemoryStore) RegisterManager(
	registrationToken string,
	name string,
	gatewayURL string,
	version string,
	capabilities *codespacev1.ManagerCapabilities,
) (*Manager, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tokenRecord, ok := s.registrationTokens[registrationToken]
	if !ok || !tokenRecord.IsActive {
		return nil, "", fmt.Errorf("registration token invalid: %w", ErrUnauthorized)
	}
	if tokenRecord.ExpiresAt != nil && tokenRecord.ExpiresAt.Before(time.Now()) {
		return nil, "", fmt.Errorf("registration token expired: %w", ErrUnauthorized)
	}

	now := time.Now()
	tokenRecord.LastUsedAt = &now

	managerUUID, err := randomToken("mgr")
	if err != nil {
		return nil, "", fmt.Errorf("random manager uuid: %w", err)
	}
	managerToken, err := randomToken("mt")
	if err != nil {
		return nil, "", fmt.Errorf("random manager token: %w", err)
	}

	manager := &Manager{
		ID:                 s.nextManagerID,
		UUID:               managerUUID,
		Name:               name,
		GatewayURL:         gatewayURL,
		Version:            version,
		Status:             codespacev1.ManagerStatus_MANAGER_STATUS_ONLINE,
		MaxConcurrency:     capabilities.GetMaxConcurrency(),
		CurrentConcurrency: capabilities.GetCurrentConcurrency(),
		LastOnlineAt:       now,
		Token:              managerToken,
		Capabilities:       cloneCapabilities(capabilities),
	}
	s.nextManagerID++
	s.managers[manager.UUID] = manager
	s.managersByID[manager.ID] = manager

	return cloneManager(manager), managerToken, nil
}

// ListManagers returns all managers.
func (s *MemoryStore) ListManagers() []*Manager {
	s.mu.Lock()
	defer s.mu.Unlock()

	values := make([]*Manager, 0, len(s.managers))
	for _, manager := range s.managers {
		values = append(values, cloneManager(manager))
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].ID < values[j].ID
	})
	return values
}

// AuthenticateManager validates one manager bearer token.
func (s *MemoryStore) AuthenticateManager(managerUUID string, managerToken string) (*Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	manager, ok := s.managers[managerUUID]
	if !ok || manager.Token != managerToken {
		return nil, fmt.Errorf("manager auth failed: %w", ErrUnauthorized)
	}
	if manager.Status == codespacev1.ManagerStatus_MANAGER_STATUS_DISABLED {
		return nil, fmt.Errorf("manager disabled: %w", ErrUnauthorized)
	}
	return cloneManager(manager), nil
}

// UpdateManagerHeartbeat updates one manager heartbeat.
func (s *MemoryStore) UpdateManagerHeartbeat(
	managerUUID string,
	status codespacev1.ManagerStatus,
	currentConcurrency int32,
	load float64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	manager, ok := s.managers[managerUUID]
	if !ok {
		return fmt.Errorf("manager %s: %w", managerUUID, ErrNotFound)
	}

	manager.Status = status
	manager.CurrentConcurrency = currentConcurrency
	manager.Load = load
	manager.LastOnlineAt = time.Now()
	manager.UpdatedCapabilities()

	return nil
}

// UpdatedCapabilities recomputes manager summary fields from capabilities.
func (m *Manager) UpdatedCapabilities() {
	if m.Capabilities == nil {
		return
	}
	m.MaxConcurrency = m.Capabilities.GetMaxConcurrency()
}

// DeclareManager updates one manager declaration and capability payload.
func (s *MemoryStore) DeclareManager(
	managerUUID string,
	name string,
	gatewayURL string,
	version string,
	capabilities *codespacev1.ManagerCapabilities,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	manager, ok := s.managers[managerUUID]
	if !ok {
		return fmt.Errorf("manager %s: %w", managerUUID, ErrNotFound)
	}

	if strings.TrimSpace(name) != "" {
		manager.Name = name
	}
	manager.GatewayURL = gatewayURL
	manager.Version = version
	manager.Capabilities = cloneCapabilities(capabilities)
	manager.MaxConcurrency = capabilities.GetMaxConcurrency()
	manager.CurrentConcurrency = capabilities.GetCurrentConcurrency()
	manager.UpdatedCapabilities()

	return nil
}

// CreateCodespace creates one codespace and its create task.
func (s *MemoryStore) CreateCodespace(input CreateCodespaceInput) (*Codespace, *Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	manager, err := s.selectManager(input.ManagerID)
	if err != nil {
		return nil, nil, err
	}

	codespaceID, err := randomToken("ws")
	if err != nil {
		return nil, nil, fmt.Errorf("random codespace id: %w", err)
	}

	repoFullName := strings.Trim(strings.TrimSpace(input.Owner)+"/"+strings.TrimSpace(input.RepoName), "/")
	if input.RepoURL == "" {
		input.RepoURL = fmt.Sprintf("https://gitea.example.com/%s.git", repoFullName)
	}
	if input.RefType == "" {
		input.RefType = "branch"
	}
	if input.RefName == "" {
		input.RefName = "main"
	}
	if input.InstanceType == "" {
		input.InstanceType = "container"
	}
	if input.Image == "" {
		input.Image = "images:debian/12"
	}
	if input.ResourcePreset == "" {
		input.ResourcePreset = "small"
	}
	if input.InitScript == "" {
		input.InitScript = manager.Capabilities.GetDefaultInitScript()
	}
	now := time.Now()
	codespace := &Codespace{
		ID:             codespaceID,
		Owner:          input.Owner,
		RepoName:       input.RepoName,
		RepoFullName:   repoFullName,
		RepoURL:        input.RepoURL,
		UserID:         input.UserID,
		RepoID:         input.RepoID,
		RefType:        input.RefType,
		RefName:        input.RefName,
		CommitSHA:      input.CommitSHA,
		PullID:         input.PullID,
		TargetBranch:   input.TargetBranch,
		HeadBranch:     input.HeadBranch,
		HeadRepo:       input.HeadRepo,
		ManagerID:      manager.ID,
		ManagerUUID:    manager.UUID,
		InstanceName:   "gitea-codespace-" + codespaceID,
		InstanceType:   input.InstanceType,
		Image:          input.Image,
		ResourcePreset: input.ResourcePreset,
		Status:         codespacev1.CodespaceStatus_CODESPACE_STATUS_INITIALIZING,
		GatewayURL:     manager.GatewayURL,
		LastActiveAt:   now,
		CreatedAt:      now,
		UpdatedAt:      now,
		Ports:          make(map[string]*Port),
	}
	s.codespace[codespace.ID] = codespace

	task := s.newTaskLocked(codespace, codespacev1.OperationType_OPERATION_TYPE_CREATE, 50, input.InitScript)
	return cloneCodespace(codespace), cloneTask(task), nil
}

// QueueCodespaceAction enqueues one lifecycle action for a codespace.
func (s *MemoryStore) QueueCodespaceAction(codespaceID string, taskType codespacev1.OperationType) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}

	switch taskType {
	case codespacev1.OperationType_OPERATION_TYPE_RESUME:
		codespace.Status = codespacev1.CodespaceStatus_CODESPACE_STATUS_INITIALIZING
	case codespacev1.OperationType_OPERATION_TYPE_STOP:
		codespace.Status = codespacev1.CodespaceStatus_CODESPACE_STATUS_STOPPING
	case codespacev1.OperationType_OPERATION_TYPE_DELETE:
		codespace.Status = codespacev1.CodespaceStatus_CODESPACE_STATUS_DELETING
	default:
		return nil, fmt.Errorf("unsupported task type %v: %w", taskType, ErrConflict)
	}

	task := s.newTaskLocked(codespace, taskType, taskPriority(taskType), codespace.RuntimePhase())
	return cloneTask(task), nil
}

func (s *MemoryStore) newTaskLocked(codespace *Codespace, taskType codespacev1.OperationType, priority int, initScript string) *Task {
	now := time.Now()
	taskID, _ := randomToken("task")
	operationID, _ := randomToken("op")
	task := &Task{
		ID:          taskID,
		OperationID: operationID,
		CodespaceID: codespace.ID,
		ManagerUUID: codespace.ManagerUUID,
		Type:        taskType,
		Status:      codespacev1.OperationStatus_OPERATION_STATUS_QUEUED,
		Priority:    priority,
		Payload: &codespacev1.CodespaceOperationPayload{
			RepoUrl:        codespace.RepoURL,
			RepoFullName:   codespace.RepoFullName,
			StartRef:       codespaceStartRef(codespace.RefType, codespace.RefName, codespace.PullID),
			StartSha:       codespace.CommitSHA,
			InstanceName:   codespace.InstanceName,
			InstanceType:   codespace.InstanceType,
			Image:          codespace.Image,
			ResourcePreset: codespace.ResourcePreset,
			InitScript:     initScript,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tasks[task.ID] = task
	codespace.ActiveOperationID = operationID
	codespace.UpdatedAt = now
	return task
}

// RuntimePhase returns the current runtime phase.
func (w *Codespace) RuntimePhase() string {
	if w.Runtime == nil {
		return ""
	}
	return w.Runtime.Phase
}

// ListCodespace returns all codespace.
func (s *MemoryStore) ListCodespace() []*Codespace {
	s.mu.Lock()
	defer s.mu.Unlock()

	values := make([]*Codespace, 0, len(s.codespace))
	for _, codespace := range s.codespace {
		values = append(values, cloneCodespace(codespace))
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].CreatedAt.Before(values[j].CreatedAt)
	})
	return values
}

// GetCodespace returns one codespace.
func (s *MemoryStore) GetCodespace(codespaceID string) (*Codespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	return cloneCodespace(codespace), nil
}

// FetchTasks leases queued tasks for one manager.
func (s *MemoryStore) FetchTasks(managerUUID string, capacity int32) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	values := make([]*Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		if task.ManagerUUID != managerUUID {
			continue
		}
		if task.Status == codespacev1.OperationStatus_OPERATION_STATUS_LEASED && task.LeaseDeadline != nil && task.LeaseDeadline.Before(now) {
			task.Status = codespacev1.OperationStatus_OPERATION_STATUS_QUEUED
			task.LeaseDeadline = nil
		}
		if task.Status == codespacev1.OperationStatus_OPERATION_STATUS_QUEUED {
			values = append(values, task)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Priority == values[j].Priority {
			return values[i].CreatedAt.Before(values[j].CreatedAt)
		}
		return values[i].Priority > values[j].Priority
	})

	if capacity <= 0 {
		capacity = 1
	}
	if int(capacity) < len(values) {
		values = values[:capacity]
	}
	for _, task := range values {
		deadline := now.Add(5 * time.Minute)
		task.Status = codespacev1.OperationStatus_OPERATION_STATUS_LEASED
		task.LeaseDeadline = &deadline
		task.UpdatedAt = now
	}

	result := make([]*Task, 0, len(values))
	for _, task := range values {
		result = append(result, cloneTask(task))
	}
	return result
}

// FinishTask completes one task and updates codespace snapshots.
func (s *MemoryStore) FinishTask(
	taskID string,
	operationID string,
	status codespacev1.OperationStatus,
	result *codespacev1.TaskResult,
	errorMessage string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if task.OperationID != operationID {
		return fmt.Errorf("operation %s is not active for task %s: %w", operationID, taskID, ErrUnauthorized)
	}
	codespace, ok := s.codespace[task.CodespaceID]
	if !ok {
		return fmt.Errorf("codespace %s: %w", task.CodespaceID, ErrNotFound)
	}

	now := time.Now()
	task.Status = status
	task.Result = cloneTaskResult(result)
	task.ErrorMessage = errorMessage
	task.LeaseDeadline = nil
	task.Attempts++
	task.UpdatedAt = now

	if result != nil {
		if result.GetInstanceName() != "" {
			codespace.InstanceName = result.GetInstanceName()
		}
		if result.GetWorkdir() != "" {
			codespace.Workdir = result.GetWorkdir()
		}
	}
	if status == codespacev1.OperationStatus_OPERATION_STATUS_FAILED {
		codespace.Status = codespacev1.CodespaceStatus_CODESPACE_STATUS_ERROR
		codespace.ErrorMessage = errorMessage
	} else if status == codespacev1.OperationStatus_OPERATION_STATUS_SUCCEEDED && task.Type == codespacev1.OperationType_OPERATION_TYPE_DELETE {
		now := time.Now()
		codespace.DeletedAt = &now
	}
	codespace.UpdatedAt = now
	return nil
}

// ReportCodespaceStatus updates codespace status from the manager.
func (s *MemoryStore) ReportCodespaceStatus(
	codespaceID string,
	status codespacev1.CodespaceStatus,
	lastActiveUnix int64,
	errorMessage string,
	operationID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	if codespace.ActiveOperationID != operationID {
		return fmt.Errorf("operation %s is not active: %w", operationID, ErrUnauthorized)
	}
	codespace.Status = status
	codespace.ErrorMessage = errorMessage
	codespace.UpdatedAt = time.Now()
	if lastActiveUnix > 0 {
		codespace.LastActiveAt = time.Unix(lastActiveUnix, 0)
	}
	if status == codespacev1.CodespaceStatus_CODESPACE_STATUS_STOPPED {
		value := time.Now()
		codespace.StoppedAt = &value
	}
	if status == codespacev1.CodespaceStatus_CODESPACE_STATUS_DELETING {
		value := time.Now()
		codespace.DeletedAt = &value
	}
	return nil
}

// AppendLog stores one operation log line.
func (s *MemoryStore) AppendLog(codespaceID, operationID, taskID string, managerID int64, level, message string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return 0, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	if codespace.ActiveOperationID != operationID {
		return 0, fmt.Errorf("operation %s is not active: %w", operationID, ErrUnauthorized)
	}
	task, ok := s.tasks[taskID]
	if !ok || task.OperationID != operationID {
		return 0, fmt.Errorf("task %s is not active: %w", taskID, ErrUnauthorized)
	}
	sequence := int64(len(s.logs[operationID]) + 1)
	entry := &LogEntry{
		ID:          s.nextLogID,
		CodespaceID: codespaceID,
		OperationID: operationID,
		TaskID:      taskID,
		ManagerID:   managerID,
		Level:       level,
		Message:     message,
		Sequence:    sequence,
		CreatedAt:   time.Now(),
	}
	s.nextLogID++
	s.logs[operationID] = append(s.logs[operationID], entry)
	return sequence, nil
}

// ReportCodespacePorts replaces the visible port set for one codespace.
func (s *MemoryStore) ReportCodespacePorts(codespaceID string, ports []*codespacev1.CodespacePort) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}

	codespace.Ports = make(map[string]*Port, len(ports))
	for _, port := range ports {
		codespace.Ports[port.GetName()] = &Port{
			Name:        port.GetName(),
			Port:        port.GetPort(),
			Protocol:    port.GetProtocol(),
			Visibility:  port.GetVisibility(),
			Description: port.GetDescription(),
			PublicURL:   port.GetPublicUrl(),
			Status:      port.GetStatus(),
		}
	}
	codespace.UpdatedAt = time.Now()
	return nil
}

// IssueGitToken creates one new codespace git token.
func (s *MemoryStore) IssueGitToken(codespaceID string, operationID string) (string, string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return "", "", time.Time{}, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	if codespace.ActiveOperationID != operationID {
		return "", "", time.Time{}, fmt.Errorf("operation binding mismatch: %w", ErrUnauthorized)
	}
	token, err := randomToken("git")
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("random git token: %w", err)
	}
	expiresAt := time.Now().Add(24 * time.Hour)
	codespace.GitToken = token
	codespace.GitTokenUsername = sanitizeUsername(codespace.Owner)
	codespace.GitTokenExpireAt = &expiresAt
	return codespace.GitTokenUsername, token, expiresAt, nil
}

// RevokeGitToken clears one codespace git token.
func (s *MemoryStore) RevokeGitToken(codespaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	codespace.GitToken = ""
	codespace.GitTokenExpireAt = nil
	return nil
}

// IssueAccessTicket creates one access ticket for gateway open flow.
func (s *MemoryStore) IssueAccessTicket(codespaceID string, userID int64, action string) (*AccessTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	token, err := randomToken("ticket")
	if err != nil {
		return nil, fmt.Errorf("random ticket: %w", err)
	}
	value := &AccessTicket{
		Token:       token,
		UserID:      userID,
		CodespaceID: codespaceID,
		RepoID:      codespace.RepoID,
		Action:      action,
		ExpiresAt:   time.Now().Add(60 * time.Second),
	}
	s.accessTickets[token] = value
	return cloneAccessTicket(value), nil
}

// ValidateAccessTicket validates and consumes one access ticket.
func (s *MemoryStore) ValidateAccessTicket(ticket string, action string) (*AccessTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.accessTickets[ticket]
	if !ok {
		return nil, fmt.Errorf("ticket %s: %w", ticket, ErrNotFound)
	}
	if record.Consumed || record.ExpiresAt.Before(time.Now()) || record.Action != action {
		return nil, fmt.Errorf("ticket invalid: %w", ErrUnauthorized)
	}
	record.Consumed = true
	return cloneAccessTicket(record), nil
}

// CreateSession creates one user session after open flow validation.
func (s *MemoryStore) CreateSession(codespaceID string, userID int64, repoID int64) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := randomToken("session")
	if err != nil {
		return nil, fmt.Errorf("random session token: %w", err)
	}
	value := &Session{
		Token:       token,
		UserID:      userID,
		CodespaceID: codespaceID,
		RepoID:      repoID,
		ExpiresAt:   time.Now().Add(12 * time.Hour),
	}
	s.sessions[token] = value
	return cloneSession(value), nil
}

// ValidateSession validates one session token.
func (s *MemoryStore) ValidateSession(token string, codespaceID string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[token]
	if !ok || session.ExpiresAt.Before(time.Now()) || session.CodespaceID != codespaceID {
		return nil, fmt.Errorf("session invalid: %w", ErrUnauthorized)
	}
	return cloneSession(session), nil
}

// PrepareRuntime allocates one runtime token and context.
func (s *MemoryStore) PrepareRuntime(codespaceID string, repo string, ref string, root string) (*RuntimeContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	token, err := randomToken("runtime")
	if err != nil {
		return nil, fmt.Errorf("random runtime token: %w", err)
	}
	context := &RuntimeContext{
		Token:       token,
		CodespaceID: codespaceID,
		Repo:        repo,
		Ref:         ref,
		Root:        root,
		Phase:       "initializing",
		Message:     "codespace bootstrapping",
		UpdatedAt:   time.Now(),
	}
	codespace.Runtime = context
	s.runtimeByToken[token] = context
	return cloneRuntimeContext(context), nil
}

// ClearRuntime removes runtime context for one codespace.
func (s *MemoryStore) ClearRuntime(codespaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok || codespace.Runtime == nil {
		return
	}
	delete(s.runtimeByToken, codespace.Runtime.Token)
	codespace.Runtime = nil
	codespace.Ports = make(map[string]*Port)
}

// ValidateRuntimeToken returns runtime context for one bearer token.
func (s *MemoryStore) ValidateRuntimeToken(token string) (*RuntimeContext, *Codespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	context, ok := s.runtimeByToken[token]
	if !ok {
		return nil, nil, fmt.Errorf("runtime token invalid: %w", ErrUnauthorized)
	}
	codespace, ok := s.codespace[context.CodespaceID]
	if !ok {
		return nil, nil, fmt.Errorf("codespace %s: %w", context.CodespaceID, ErrNotFound)
	}
	return cloneRuntimeContext(context), cloneCodespace(codespace), nil
}

// UpdateRuntimeStatus updates runtime phase and message.
func (s *MemoryStore) UpdateRuntimeStatus(token string, phase string, message string) (*RuntimeContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	context, ok := s.runtimeByToken[token]
	if !ok {
		return nil, fmt.Errorf("runtime token invalid: %w", ErrUnauthorized)
	}
	context.Phase = phase
	context.Message = message
	context.UpdatedAt = time.Now()
	if codespace, ok := s.codespace[context.CodespaceID]; ok {
		codespace.Runtime = context
		codespace.UpdatedAt = time.Now()
	}
	return cloneRuntimeContext(context), nil
}

// UpsertRuntimePort creates or replaces one runtime port.
func (s *MemoryStore) UpsertRuntimePort(token string, port Port) (*Port, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	context, ok := s.runtimeByToken[token]
	if !ok {
		return nil, fmt.Errorf("runtime token invalid: %w", ErrUnauthorized)
	}
	codespace, ok := s.codespace[context.CodespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", context.CodespaceID, ErrNotFound)
	}
	if codespace.Ports == nil {
		codespace.Ports = make(map[string]*Port)
	}
	value := port
	codespace.Ports[port.Name] = &value
	codespace.UpdatedAt = time.Now()
	return clonePort(&value), nil
}

// PatchRuntimePort updates one runtime port.
func (s *MemoryStore) PatchRuntimePort(token string, name string, visibility codespacev1.PortVisibility, description string) (*Port, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	context, ok := s.runtimeByToken[token]
	if !ok {
		return nil, fmt.Errorf("runtime token invalid: %w", ErrUnauthorized)
	}
	codespace, ok := s.codespace[context.CodespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", context.CodespaceID, ErrNotFound)
	}
	port, ok := codespace.Ports[name]
	if !ok {
		return nil, fmt.Errorf("port %s: %w", name, ErrNotFound)
	}
	if visibility != codespacev1.PortVisibility_PORT_VISIBILITY_UNSPECIFIED {
		port.Visibility = visibility
	}
	if description != "" {
		port.Description = description
	}
	return clonePort(port), nil
}

// DeleteRuntimePort deletes one runtime port.
func (s *MemoryStore) DeleteRuntimePort(token string, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	context, ok := s.runtimeByToken[token]
	if !ok {
		return fmt.Errorf("runtime token invalid: %w", ErrUnauthorized)
	}
	codespace, ok := s.codespace[context.CodespaceID]
	if !ok {
		return fmt.Errorf("codespace %s: %w", context.CodespaceID, ErrNotFound)
	}
	delete(codespace.Ports, name)
	codespace.UpdatedAt = time.Now()
	return nil
}

// FindPort returns one port by codespace and name.
func (s *MemoryStore) FindPort(codespaceID string, name string) (*Port, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codespace, ok := s.codespace[codespaceID]
	if !ok {
		return nil, fmt.Errorf("codespace %s: %w", codespaceID, ErrNotFound)
	}
	port, ok := codespace.Ports[name]
	if !ok {
		return nil, fmt.Errorf("port %s: %w", name, ErrNotFound)
	}
	return clonePort(port), nil
}

func (s *MemoryStore) selectManager(managerID int64) (*Manager, error) {
	if managerID > 0 {
		manager, ok := s.managersByID[managerID]
		if !ok {
			return nil, fmt.Errorf("manager %d: %w", managerID, ErrNotFound)
		}
		if manager.Status != codespacev1.ManagerStatus_MANAGER_STATUS_ONLINE {
			return nil, fmt.Errorf("manager %d offline: %w", managerID, ErrConflict)
		}
		return manager, nil
	}
	for _, manager := range s.managers {
		if manager.Status == codespacev1.ManagerStatus_MANAGER_STATUS_ONLINE {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no online manager: %w", ErrNotFound)
}

func taskPriority(taskType codespacev1.OperationType) int {
	switch taskType {
	case codespacev1.OperationType_OPERATION_TYPE_DELETE:
		return 100
	case codespacev1.OperationType_OPERATION_TYPE_STOP:
		return 80
	case codespacev1.OperationType_OPERATION_TYPE_RESUME:
		return 60
	default:
		return 50
	}
}

func codespaceStartRef(refType, refName string, pullID int64) string {
	switch strings.ToLower(strings.TrimSpace(refType)) {
	case "pull":
		if pullID > 0 {
			return fmt.Sprintf("refs/pull/%d/head", pullID)
		}
		return strings.TrimSpace(refName)
	case "tag":
		if refName == "" {
			return ""
		}
		return "refs/tags/" + refName
	case "commit":
		return ""
	default:
		if refName == "" {
			return ""
		}
		return "refs/heads/" + refName
	}
}

func sanitizeUsername(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "codespace"
	}
	value = strings.ReplaceAll(value, "/", "-")
	return value
}

func randomToken(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buffer), nil
}

func cloneCapabilities(value *codespacev1.ManagerCapabilities) *codespacev1.ManagerCapabilities {
	if value == nil {
		return nil
	}
	return proto.Clone(value).(*codespacev1.ManagerCapabilities)
}

func cloneRegistrationToken(value *RegistrationToken) *RegistrationToken {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneManager(value *Manager) *Manager {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Capabilities = cloneCapabilities(value.Capabilities)
	return &copyValue
}

func cloneRuntimeContext(value *RuntimeContext) *RuntimeContext {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func clonePort(value *Port) *Port {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneCodespace(value *Codespace) *Codespace {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Runtime = cloneRuntimeContext(value.Runtime)
	if value.Ports != nil {
		copyValue.Ports = make(map[string]*Port, len(value.Ports))
		for name, port := range value.Ports {
			copyValue.Ports[name] = clonePort(port)
		}
	}
	return &copyValue
}

func cloneTask(value *Task) *Task {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Result = cloneTaskResult(value.Result)
	if value.Payload != nil {
		copyValue.Payload = proto.Clone(value.Payload).(*codespacev1.CodespaceOperationPayload)
	}
	return &copyValue
}

func cloneTaskResult(value *codespacev1.TaskResult) *codespacev1.TaskResult {
	if value == nil {
		return nil
	}
	return proto.Clone(value).(*codespacev1.TaskResult)
}

func cloneAccessTicket(value *AccessTicket) *AccessTicket {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneSession(value *Session) *Session {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
