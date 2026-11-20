package authorization

import "context"

// Inspect consults the integration's native store, or the ordinary Go store.
// Integration callbacks always run outside the service mutex.
func (s *Service) Inspect(ctx context.Context, key string) (AccountState, error) {
	s.mu.Lock()
	f := s.flows[key]
	s.mu.Unlock()
	if f == nil {
		return AccountState{}, failure("NO_FLOW", "Authorization flow is unavailable")
	}
	if f.Inspect != nil {
		return f.Inspect(ctx)
	}
	info, err := s.store.DescribeRecord(key)
	return AccountState{Configured: info.Configured}, err
}
func (s *Service) Disconnect(ctx context.Context, key string) (AccountState, error) {
	s.mu.Lock()
	f := s.flows[key]
	if f == nil || f.Disconnect == nil {
		s.mu.Unlock()
		return AccountState{}, failure("NO_FLOW", "This integration does not support disconnect")
	}
	if s.running[key] != nil || s.disconnecting[key] {
		s.mu.Unlock()
		return AccountState{}, failure("ALREADY_IN_FLIGHT", "Authorization is already in progress")
	}
	s.disconnecting[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.disconnecting, key); s.mu.Unlock() }()
	if err := f.Disconnect(ctx); err != nil {
		return AccountState{}, err
	}
	state, err := s.Inspect(ctx, key)
	if err == nil && state.Configured {
		return state, failure("STILL_CONFIGURED", "The application still reports an authorized account")
	}
	if err == nil {
		s.settled(key, "disconnected")
	}
	return state, err
}
