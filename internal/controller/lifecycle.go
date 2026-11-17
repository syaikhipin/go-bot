package controller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go-bot/runtime"
)

type HostStatus struct {
	HostID string `json:"hostId,omitempty"`
	Status string `json:"status"`
}

func (m *Manager) Observe(ctx context.Context, id string) (HostStatus, error) {
	record, err := m.Store.Get(ctx, id)
	if err != nil {
		return HostStatus{}, err
	}
	return m.hostStatus(ctx, record), nil
}

func (m *Manager) hostStatus(ctx context.Context, record Account) HostStatus {
	if record.DesiredState != running {
		return HostStatus{HostID: record.HostID, Status: stopped}
	}
	status := "starting"
	if record.HostEndpoint != "" {
		status = "unavailable"
		if m.probe(ctx, record.HostEndpoint, record.HostToken) == nil {
			status = "ready"
		}
	}
	return HostStatus{HostID: record.HostID, Status: status}
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	return m.stopHost(ctx, id, !m.Driver.Capabilities().NativeStop)
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	return m.stopHost(ctx, id, true)
}

func (m *Manager) stopHost(ctx context.Context, id string, release bool) (result error) {
	lock := m.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.Config.Runtime.StartTimeout)
	defer cancel()
	defer func() {
		if result != nil && ctx.Err() != nil {
			result = ctx.Err()
		}
	}()
	record, err := m.Store.Get(ctx, id)
	if err != nil || record.HostID == "" {
		return err
	}
	instance, err := m.find(ctx, id, record)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if err == nil {
		if release {
			if !m.Driver.Capabilities().NativeStop && instance.State == running {
				m.gracefulExit(ctx, instance, record.HostToken)
			}
			if err := m.Driver.Delete(ctx, instance.Ref); err != nil {
				return err
			}
		} else if instance.State != stopped {
			if err := m.Driver.Stop(ctx, instance.Ref); err != nil {
				return err
			}
		}
		if err = m.waitForStop(ctx, instance.Ref, release); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	if release || errors.Is(err, runtime.ErrNotFound) {
		record.HostID, record.HostToken = "", ""
	}
	record.DesiredState = stopped
	record.HostEndpoint = ""
	return m.Store.Save(ctx, id, record)
}

func (m *Manager) gracefulExit(ctx context.Context, instance runtime.Instance, token string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := m.control(ctx, instance.Endpoint, token, "/internal/host/shutdown", http.StatusAccepted); err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		observed, err := m.Driver.Inspect(ctx, instance.Ref)
		if errors.Is(err, runtime.ErrNotFound) || (err == nil && observed.State == stopped) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) waitForStop(ctx context.Context, ref runtime.ResourceRef, release bool) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		instance, err := m.Driver.Inspect(ctx, ref)
		if err != nil {
			return err
		}
		if !release && instance.State == stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
