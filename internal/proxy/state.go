package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const stateVersion = 1

// savedState is the on-disk routing table. It lets a restarted proxy
// container (host reboot, image upgrade) resume routing to the containers
// that were live, without waiting for the next deploy.
type savedState struct {
	Version  int            `json:"version"`
	Services []savedService `json:"services"`
}

type savedService struct {
	Settings    DeployRequest `json:"settings"`
	Maintenance bool          `json:"maintenance"`
}

func (s *Server) loadState() error {
	content, err := os.ReadFile(s.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read proxy state: %w", err)
	}
	var state savedState
	if err := json.Unmarshal(content, &state); err != nil {
		return fmt.Errorf("decode proxy state %s: %w", s.statePath, err)
	}
	if state.Version != stateVersion {
		return fmt.Errorf("proxy state %s has unsupported version %d", s.statePath, state.Version)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, saved := range state.Services {
		settings := saved.Settings
		if err := settings.normalize(); err != nil {
			return fmt.Errorf("proxy state service %q: %w", saved.Settings.Service, err)
		}
		if err := s.checkHostsLocked(settings.Service, settings.Hosts); err != nil {
			return fmt.Errorf("proxy state: %w", err)
		}
		svc := &service{name: settings.Service}
		svc.settings.Store(&settings)
		svc.target.Store(newTarget(settings.Target, &settings, s.logger))
		svc.maintenance.Store(saved.Maintenance)
		s.services[settings.Service] = svc
		s.reindexLocked()
	}
	return nil
}

// saveState writes the routing table atomically (temporary file, fsync,
// rename) with 0600 permissions.
func (s *Server) saveState() error {
	if s.statePath == "" {
		return nil
	}
	s.mu.RLock()
	state := savedState{Version: stateVersion, Services: []savedService{}}
	for _, status := range s.statusLocked() {
		svc := s.services[status.Service]
		settings := *svc.settings.Load()
		if t := svc.target.Load(); t != nil {
			settings.Target = t.addr
		}
		state.Services = append(state.Services, savedService{Settings: settings, Maintenance: svc.maintenance.Load()})
	}
	s.mu.RUnlock()
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	dir := filepath.Dir(s.statePath)
	temp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("write proxy state: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := temp.Write(append(content, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write proxy state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write proxy state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write proxy state: %w", err)
	}
	if err := os.Rename(tempName, s.statePath); err != nil {
		return fmt.Errorf("write proxy state: %w", err)
	}
	return nil
}
