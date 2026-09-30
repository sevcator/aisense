package config

import "testing"

func TestTorConfigRejectsManagedPortOverrides(t *testing.T) {
	for _, directive := range []string{"SocksPort 0.0.0.0:9050", "DataDirectory /tmp/other", "ControlPort 9051", "RunAsDaemon 1"} {
		if err := ValidateTorCfg(TorCfg{Enabled: true, SOCKSPort: 9050, RestartAfter: 10, Config: directive}); err == nil {
			t.Fatalf("accepted %q", directive)
		}
	}
	if err := ValidateTorCfg(TorCfg{Enabled: true, SOCKSPort: 9050, RestartAfter: 10, Config: "UseBridges 1\nBridge obfs4 example"}); err != nil {
		t.Fatal(err)
	}
}
