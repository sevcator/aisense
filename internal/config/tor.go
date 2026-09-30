package config

import (
	"fmt"
	"strings"
)

func ValidateTorCfg(c TorCfg) error {
	if !c.Enabled && c.SOCKSPort == 0 && c.RestartAfter == 0 && c.Config == "" {
		return nil
	}
	if c.SOCKSPort < 1 || c.SOCKSPort > 65535 {
		return fmt.Errorf("Tor SOCKS port must be between 1 and 65535")
	}
	if c.RestartAfter < 1 || c.RestartAfter > 1000000 {
		return fmt.Errorf("Tor restart request count must be between 1 and 1000000")
	}
	for _, line := range strings.Split(c.Config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "socksport", "datadirectory", "controlport", "runasdaemon", "pidfile":
			return fmt.Errorf("Tor directive %s is managed by aisense", fields[0])
		}
	}
	return nil
}
