// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import "testing"

func saveManagerRegistrationForTest(t *testing.T, stateDir, giteaURL string, managerID int64) {
	t.Helper()

	if err := SaveManagerIdentity(stateDir, ManagerIdentity{
		GiteaURL:       giteaURL,
		ManagerID:      managerID,
		RegisteredUnix: 1,
	}); err != nil {
		t.Fatalf("save manager identity: %v", err)
	}
	if err := SaveManagerCredentials(stateDir, ManagerCredentials{ManagerSecret: "manager-secret"}); err != nil {
		t.Fatalf("save manager credentials: %v", err)
	}
	if err := SaveManagerRootState(stateDir, ManagerRootState{ManagerID: managerID}); err != nil {
		t.Fatalf("save manager root state: %v", err)
	}
}
