package models

import "fmt"

// GetPort returns the storage HTTP port (default 8888).
func (s *Storage) GetPort() int {
	if s.Local != nil && s.Local.Port > 0 {
		return s.Local.Port
	}
	return 8888
}

// GetHostPort returns "host:port" for internal HTTP access (storage static server).
func (s *Storage) GetHostPort() string {
	host := s.GetHost()
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, s.GetPort())
}
