package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexSessionFailoverSettingsRuntime(test *testing.T) {
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	if DefaultRuntimeSettings().CodexSessionFailoverEnabled || NormalizeRuntimeSettings(RuntimeSettings{}).CodexSessionFailoverEnabled {
		test.Fatal("session failover must default off")
	}
	for _, enabled := range []bool{true, false} {
		next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{CodexSessionFailoverEnabled: enabled})
		if next.CodexSessionFailoverEnabled != enabled || CurrentRuntimeSettings().CodexSessionFailoverEnabled != enabled {
			test.Fatalf("persisted failover setting did not reach runtime: want %t", enabled)
		}
		next = UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
			current.CodexCapacityRetryEnabled = true
			return current
		})
		if next.CodexSessionFailoverEnabled != enabled {
			test.Fatal("unrelated runtime update changed session failover")
		}
	}
	ApplyRuntimeSettings(RuntimeSettings{CodexSessionFailoverEnabled: true})
	if ApplyRuntimeSettingsFromSystem(nil).CodexSessionFailoverEnabled || CurrentRuntimeSettings().CodexSessionFailoverEnabled {
		test.Fatal("missing system settings must reset session failover to off")
	}
}
