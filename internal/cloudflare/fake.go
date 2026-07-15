package cloudflare

import (
	"context"
	"sync"
)

// Fake is an in-memory API implementation for tests.
type Fake struct {
	mu sync.Mutex

	Config      TunnelConfig
	Records     map[string]bool
	UpdateCalls int

	GetErr    error
	UpdateErr error
	EnsureErr map[string]error
	DeleteErr map[string]error
}

var _ API = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{Records: map[string]bool{}}
}

func (f *Fake) GetTunnelConfig(_ context.Context) (*TunnelConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	cfg := TunnelConfig{Ingress: append([]IngressRule(nil), f.Config.Ingress...)}
	return &cfg, nil
}

func (f *Fake) UpdateTunnelConfig(_ context.Context, cfg *TunnelConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UpdateErr != nil {
		return f.UpdateErr
	}
	f.UpdateCalls++
	f.Config = TunnelConfig{Ingress: append([]IngressRule(nil), cfg.Ingress...)}
	return nil
}

func (f *Fake) EnsureDNSRecord(_ context.Context, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.EnsureErr[hostname]; err != nil {
		return err
	}
	f.Records[hostname] = true
	return nil
}

func (f *Fake) DeleteDNSRecord(_ context.Context, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.DeleteErr[hostname]; err != nil {
		return err
	}
	delete(f.Records, hostname)
	return nil
}
