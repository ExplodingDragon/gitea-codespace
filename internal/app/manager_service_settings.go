// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import "gitea.dev/codespace/internal/manager"

type managerServiceSettingsStores []manager.ManagerServiceSettingsStore

func (stores managerServiceSettingsStores) SaveManagerServiceSettings(settings manager.ManagerServiceSettings) error {
	for _, store := range stores {
		if store == nil {
			continue
		}
		if err := store.SaveManagerServiceSettings(settings); err != nil {
			return err
		}
	}
	return nil
}
