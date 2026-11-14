// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	gatewaySessionTicketVersion  = 1
	gatewaySessionTicketMaxBytes = 4096
	gatewaySessionTicketAAD      = "gitea-codespace-gateway-session-v1"
)

type gatewaySessionTicket struct {
	Version       int    `json:"v"`
	ID            string `json:"id"`
	SiteUID       string `json:"site"`
	UserID        int64  `json:"user"`
	CodespaceUUID string `json:"codespace"`
	EndpointID    string `json:"endpoint"`
	IssuedAt      int64  `json:"issued"`
	ExpiresAt     int64  `json:"expires"`
	IdleExpiresAt int64  `json:"idleExpires"`
}

type gatewaySessionTickets struct {
	aead cipher.AEAD
}

func newGatewaySessionTickets(key []byte) (*gatewaySessionTickets, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("gateway session key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &gatewaySessionTickets{aead: aead}, nil
}

func (t *gatewaySessionTickets) seal(ticket gatewaySessionTicket) (string, error) {
	plain, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, t.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate gateway session nonce: %w", err)
	}
	sealed := t.aead.Seal(nonce, nonce, plain, []byte(gatewaySessionTicketAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (t *gatewaySessionTickets) open(token string) (gatewaySessionTicket, bool) {
	if t == nil || token == "" || len(token) > gatewaySessionTicketMaxBytes {
		return gatewaySessionTicket{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) <= t.aead.NonceSize() {
		return gatewaySessionTicket{}, false
	}
	nonce, ciphertext := sealed[:t.aead.NonceSize()], sealed[t.aead.NonceSize():]
	plain, err := t.aead.Open(nil, nonce, ciphertext, []byte(gatewaySessionTicketAAD))
	if err != nil {
		return gatewaySessionTicket{}, false
	}
	var ticket gatewaySessionTicket
	if json.Unmarshal(plain, &ticket) != nil || ticket.Version != gatewaySessionTicketVersion || ticket.ID == "" || ticket.SiteUID == "" || ticket.UserID <= 0 || ticket.CodespaceUUID == "" || ticket.EndpointID == "" || ticket.IssuedAt <= 0 || ticket.ExpiresAt <= ticket.IssuedAt || ticket.IdleExpiresAt <= ticket.IssuedAt || ticket.IdleExpiresAt > ticket.ExpiresAt {
		return gatewaySessionTicket{}, false
	}
	return ticket, true
}

func (t gatewaySessionTicket) validAt(now time.Time) bool {
	return now.Before(time.UnixMilli(t.ExpiresAt)) && now.Before(time.UnixMilli(t.IdleExpiresAt))
}
