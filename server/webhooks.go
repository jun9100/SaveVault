// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	webhooksFileName   = "webhooks.json"
	maxWebhookChannels = 16
	webhookQueueSize   = 64
	webhookTimeout     = 10 * time.Second
)

const (
	webhookTypeGeneric  = "generic"
	webhookTypeDiscord  = "discord"
	webhookTypeTelegram = "telegram"
	webhookTypeGotify   = "gotify"
	webhookTypeBark     = "bark"
	webhookTypeFeishu   = "feishu"
	webhookTypeWeCom    = "wecom"
)

var validWebhookTypes = map[string]bool{
	webhookTypeGeneric:  true,
	webhookTypeDiscord:  true,
	webhookTypeTelegram: true,
	webhookTypeGotify:   true,
	webhookTypeBark:     true,
	webhookTypeFeishu:   true,
	webhookTypeWeCom:    true,
}

var validWebhookEvents = map[string]bool{
	"*":              true,
	"conflict":       true,
	"quota_warning":  true,
	"quota_exceeded": true,
	"auth_locked":    true,
	"prune":          true,
	"device_revoked": true,
	"upload":         true,
}

type webhookChannel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	URL     string   `json:"url"`
	Secret  string   `json:"secret,omitempty"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
}

type webhookChannelView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	URL       string   `json:"url"`
	HasSecret bool     `json:"has_secret"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
}

type webhookChannelInput struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	URL         string   `json:"url"`
	Secret      string   `json:"secret,omitempty"`
	ClearSecret bool     `json:"clear_secret,omitempty"`
	Events      []string `json:"events"`
	Enabled     bool     `json:"enabled"`
}

type webhookConfigState struct {
	Version  int              `json:"version"`
	Channels []webhookChannel `json:"channels"`
}

type webhookNotification struct {
	Event     string    `json:"event"`
	VaultID   string    `json:"vault_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
	Scope     string    `json:"scope,omitempty"`
	Snapshot  string    `json:"snapshot,omitempty"`
	Device    string    `json:"device,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

type webhookDelivery struct {
	channel webhookChannel
	notice  webhookNotification
}

func defaultWebhookConfig() webhookConfigState {
	return webhookConfigState{
		Version:  1,
		Channels: []webhookChannel{},
	}
}

func (s *server) webhooksPath() string {
	return filepath.Join(s.dataDir, webhooksFileName)
}

func (s *server) loadWebhooks() error {
	state := defaultWebhookConfig()
	if err := readJSONFile(s.webhooksPath(), &state); err != nil {
		return err
	}
	if state.Channels == nil {
		state.Channels = []webhookChannel{}
	}
	state.Version = 1
	s.webhookMu.Lock()
	s.webhooks = state
	s.webhookMu.Unlock()
	return nil
}

func (s *server) persistWebhooksLocked(state webhookConfigState) error {
	state.Version = 1
	return writeJSONFile(s.webhooksPath(), state)
}

func validateWebhookURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("webhook url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", errors.New("webhook url is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("webhook url must use http or https")
	}
	return trimmed, nil
}

func normalizeWebhookEvents(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{"conflict", "quota_warning", "quota_exceeded", "auth_locked"}, nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, ev := range raw {
		clean := strings.ToLower(strings.TrimSpace(ev))
		if clean == "" {
			continue
		}
		if !validWebhookEvents[clean] {
			return nil, fmt.Errorf("unsupported webhook event %q", ev)
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one webhook event is required")
	}
	return out, nil
}

func webhookEventTopic(kind string) string {
	switch kind {
	case eventConflict:
		return "conflict"
	case eventQuotaWarning:
		return "quota_warning"
	case eventQuotaExceeded:
		return "quota_exceeded"
	case eventAuthLocked:
		return "auth_locked"
	case eventPrune, eventPruneScope:
		return "prune"
	case eventDeviceTokenRevoked, eventDeviceRemoved:
		return "device_revoked"
	case eventUpload:
		return "upload"
	default:
		return ""
	}
}

func webhookChannelSubscribes(ch webhookChannel, topic string) bool {
	if !ch.Enabled {
		return false
	}
	for _, ev := range ch.Events {
		if ev == "*" || ev == topic {
			return true
		}
	}
	return false
}

func webhookSummaryForEvent(vaultID string, item event) (webhookNotification, bool) {
	topic := webhookEventTopic(item.Kind)
	if topic == "" {
		return webhookNotification{}, false
	}
	ts := item.Time
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	var title string
	switch topic {
	case "conflict":
		title = "SaveVault 存档冲突告警"
	case "quota_warning":
		title = "SaveVault 存储配额接近上限"
	case "quota_exceeded":
		title = "SaveVault 存储配额已满"
	case "auth_locked":
		title = "SaveVault 认证失败触发限流锁定"
	case "prune":
		title = "SaveVault 保留策略清理完成"
	case "device_revoked":
		title = "SaveVault 设备凭据已吊销"
	case "upload":
		title = "SaveVault 收到新存档上传"
	}
	parts := make([]string, 0, 5)
	if vaultID != "" && vaultID != defaultVaultID {
		parts = append(parts, "Vault: "+vaultID)
	}
	if item.Scope != "" {
		parts = append(parts, "范围: "+item.Scope)
	}
	if item.Device != "" {
		parts = append(parts, "设备: "+item.Device)
	}
	if item.Snapshot != "" {
		parts = append(parts, "快照: "+item.Snapshot)
	}
	if item.Detail != "" {
		parts = append(parts, item.Detail)
	}
	body := strings.Join(parts, " · ")
	if body == "" {
		body = topic
	}
	return webhookNotification{
		Event:     topic,
		VaultID:   vaultID,
		Title:     title,
		Body:      body,
		Timestamp: ts.UTC(),
		Scope:     item.Scope,
		Snapshot:  item.Snapshot,
		Device:    item.Device,
		Detail:    item.Detail,
	}, true
}

func buildWebhookRequest(ctx context.Context, ch webhookChannel, notice webhookNotification) (*http.Request, error) {
	var body []byte
	var err error
	headers := map[string]string{
		"Content-Type": "application/json; charset=utf-8",
		"User-Agent":   "SaveVault-Webhook/" + version,
	}
	switch ch.Type {
	case webhookTypeDiscord:
		color := 0x2563EB
		switch notice.Event {
		case "conflict", "quota_exceeded", "auth_locked":
			color = 0xDC2626
		case "quota_warning":
			color = 0xD97706
		}
		body, err = json.Marshal(map[string]any{
			"content": notice.Title,
			"embeds": []map[string]any{{
				"title":       notice.Title,
				"description": notice.Body,
				"color":       color,
				"timestamp":   notice.Timestamp.UTC().Format(time.RFC3339),
			}},
		})
	case webhookTypeTelegram:
		payload := map[string]any{
			"text": notice.Title + "\n" + notice.Body,
		}
		if ch.Secret != "" {
			payload["chat_id"] = ch.Secret
		}
		body, err = json.Marshal(payload)
	case webhookTypeGotify:
		body, err = json.Marshal(map[string]any{
			"title":    notice.Title,
			"message":  notice.Body,
			"priority": 5,
		})
		if ch.Secret != "" {
			headers["X-Gotify-Key"] = ch.Secret
		}
	case webhookTypeBark:
		body, err = json.Marshal(map[string]any{
			"title": notice.Title,
			"body":  notice.Body,
			"group": "SaveVault",
		})
	case webhookTypeFeishu:
		body, err = json.Marshal(map[string]any{
			"msg_type": "text",
			"content": map[string]string{
				"text": notice.Title + "\n" + notice.Body,
			},
		})
	case webhookTypeWeCom:
		body, err = json.Marshal(map[string]any{
			"msgtype": "text",
			"text": map[string]string{
				"content": notice.Title + "\n" + notice.Body,
			},
		})
	default: // generic
		body, err = json.Marshal(notice)
		if err == nil && ch.Secret != "" {
			mac := hmac.New(sha256.New, []byte(ch.Secret))
			mac.Write(body)
			headers["X-SaveVault-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		headers["X-SaveVault-Event"] = notice.Event
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func newWebhookHTTPClient() *http.Client {
	return &http.Client{
		Timeout: webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *server) sendWebhookSync(ctx context.Context, ch webhookChannel, notice webhookNotification) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := buildWebhookRequest(reqCtx, ch, notice)
	if err != nil {
		return 0, err
	}
	resp, err := s.webhookClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (s *server) ensureWebhookWorkerLocked() {
	if s.webhookStarted || s.webhookClosed {
		return
	}
	s.webhookStarted = true
	s.webhookWG.Add(1)
	go func() {
		defer s.webhookWG.Done()
		for {
			select {
			case <-s.webhookCtx.Done():
				return
			case job := <-s.webhookQueue:
				if _, err := s.sendWebhookSync(s.webhookCtx, job.channel, job.notice); err != nil {
					slog.Warn("webhook delivery failed", "channel", job.channel.Name, "type", job.channel.Type, "event", job.notice.Event, "error", err)
				}
			}
		}
	}()
}

// notifyWebhooks enqueues matching webhook notifications asynchronously without
// holding s.mu during network I/O. Safe to call while s.mu is held because
// s.webhookMu is a leaf lock.
func (s *server) notifyWebhooks(item event) {
	notice, ok := webhookSummaryForEvent(s.vaultID, item)
	if !ok {
		return
	}
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	if s.webhookClosed || len(s.webhooks.Channels) == 0 {
		return
	}
	for _, ch := range s.webhooks.Channels {
		if !webhookChannelSubscribes(ch, notice.Event) {
			continue
		}
		s.ensureWebhookWorkerLocked()
		select {
		case s.webhookQueue <- webhookDelivery{channel: ch, notice: notice}:
		default:
			slog.Warn("webhook queue full; dropping event", "channel", ch.Name, "event", notice.Event)
		}
	}
}

func (s *server) closeWebhooks() {
	s.webhookMu.Lock()
	s.webhookClosed = true
	s.webhookMu.Unlock()
	s.webhookCancel()
	s.webhookWG.Wait()
}

func webhookChannelViews(channels []webhookChannel) []webhookChannelView {
	views := make([]webhookChannelView, 0, len(channels))
	for _, ch := range channels {
		views = append(views, webhookChannelView{
			ID:        ch.ID,
			Name:      ch.Name,
			Type:      ch.Type,
			URL:       ch.URL,
			HasSecret: ch.Secret != "",
			Events:    append([]string(nil), ch.Events...),
			Enabled:   ch.Enabled,
		})
	}
	return views
}

func (s *server) handleGetWebhooks(w http.ResponseWriter, _ *http.Request) {
	s.webhookMu.Lock()
	views := webhookChannelViews(s.webhooks.Channels)
	s.webhookMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": views,
		"supported_types": []string{
			webhookTypeGeneric, webhookTypeDiscord, webhookTypeTelegram,
			webhookTypeGotify, webhookTypeBark, webhookTypeFeishu, webhookTypeWeCom,
		},
		"supported_events": []string{
			"conflict", "quota_warning", "quota_exceeded",
			"auth_locked", "prune", "device_revoked", "upload", "*",
		},
	})
}

func (s *server) handlePutWebhooks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channels []webhookChannelInput `json:"channels"`
	}
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	if len(body.Channels) > maxWebhookChannels {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d webhook channels are supported", maxWebhookChannels))
		return
	}

	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	existingSecrets := make(map[string]string, len(s.webhooks.Channels))
	for _, ch := range s.webhooks.Channels {
		existingSecrets[ch.ID] = ch.Secret
	}

	next := make([]webhookChannel, 0, len(body.Channels))
	seenIDs := make(map[string]struct{}, len(body.Channels))
	for _, raw := range body.Channels {
		chType := strings.ToLower(strings.TrimSpace(raw.Type))
		if chType == "" {
			chType = webhookTypeGeneric
		}
		if !validWebhookTypes[chType] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported webhook type %q", raw.Type))
			return
		}
		cleanURL, err := validateWebhookURL(raw.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		events, err := normalizeWebhookEvents(raw.Events)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		id := strings.TrimSpace(raw.ID)
		if id == "" || !validIdentifier(id, 48) {
			gen, genErr := randomHex(8)
			if genErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot allocate webhook id")
				return
			}
			id = "wh_" + gen
		}
		if _, dup := seenIDs[id]; dup {
			writeError(w, http.StatusBadRequest, "duplicate webhook channel id")
			return
		}
		seenIDs[id] = struct{}{}
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			name = chType
		}
		if len([]rune(name)) > 80 {
			name = string([]rune(name)[:80])
		}
		secret := strings.TrimSpace(raw.Secret)
		if secret == "" && !raw.ClearSecret {
			secret = existingSecrets[id]
		}
		next = append(next, webhookChannel{
			ID:      id,
			Name:    name,
			Type:    chType,
			URL:     cleanURL,
			Secret:  secret,
			Events:  events,
			Enabled: raw.Enabled,
		})
	}

	updated := webhookConfigState{Version: 1, Channels: next}
	if err := s.persistWebhooksLocked(updated); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save webhook configuration")
		return
	}
	s.webhooks = updated
	writeJSON(w, http.StatusOK, map[string]any{"channels": webhookChannelViews(next)})
}

func (s *server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id,omitempty"`
		Type   string `json:"type,omitempty"`
		URL    string `json:"url,omitempty"`
		Secret string `json:"secret,omitempty"`
		Name   string `json:"name,omitempty"`
	}
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var ch webhookChannel
	if id := strings.TrimSpace(body.ID); id != "" {
		s.webhookMu.Lock()
		found := false
		for _, candidate := range s.webhooks.Channels {
			if candidate.ID == id {
				ch = candidate
				found = true
				break
			}
		}
		s.webhookMu.Unlock()
		if !found {
			writeError(w, http.StatusNotFound, "webhook channel not found")
			return
		}
	} else {
		chType := strings.ToLower(strings.TrimSpace(body.Type))
		if chType == "" {
			chType = webhookTypeGeneric
		}
		if !validWebhookTypes[chType] {
			writeError(w, http.StatusBadRequest, "unsupported webhook type")
			return
		}
		cleanURL, err := validateWebhookURL(body.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ch = webhookChannel{
			Name:    strings.TrimSpace(body.Name),
			Type:    chType,
			URL:     cleanURL,
			Secret:  strings.TrimSpace(body.Secret),
			Enabled: true,
		}
	}
	notice := webhookNotification{
		Event:     "test",
		VaultID:   s.vaultID,
		Title:     "SaveVault Webhook 测试消息",
		Body:      fmt.Sprintf("来自 Vault %s 的连通性测试（版本 %s）", s.vaultID, version),
		Timestamp: s.now().UTC(),
		Detail:    "test delivery",
	}
	statusCode, err := s.sendWebhookSync(r.Context(), ch, notice)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          false,
			"status_code": statusCode,
			"error":       err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"status_code": statusCode,
	})
}
