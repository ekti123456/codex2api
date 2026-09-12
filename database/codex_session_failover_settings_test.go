package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexSessionFailoverSettingsSQLiteDefaultsAndMigration(test *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(test.TempDir(), "session-failover.db")
	db, err := New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	if _, err := db.conn.ExecContext(ctx, "INSERT INTO system_settings (id) VALUES (1)"); err != nil {
		test.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexSessionFailoverEnabled {
		test.Fatalf("fresh setting must default off: settings=%+v err=%v", settings, err)
	}
	if _, err := db.conn.ExecContext(ctx, "UPDATE system_settings SET codex_session_failover_enabled = NULL WHERE id = 1"); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexSessionFailoverEnabled {
		test.Fatalf("null setting must default off: settings=%+v err=%v", settings, err)
	}
	settings.SiteName = "existing installation"
	settings.CodexImagesMainModel = "gpt-5.6-luna"
	settings.CodexTelemetryEnabled = true
	settings.CodexSessionFailoverEnabled = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		test.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_session_failover_enabled"); err != nil {
		test.Fatal(err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		test.Fatalf("read migrated settings: %v", err)
	}
	if settings.CodexSessionFailoverEnabled || settings.SiteName != "existing installation" || settings.CodexImagesMainModel != "gpt-5.6-luna" || !settings.CodexTelemetryEnabled {
		test.Fatal("migration enabled failover or changed existing settings")
	}
	testCodexSessionFailoverSettingsRoundTrip(test, db)
}

func TestCodexSessionFailoverSettingsPostgres(test *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		test.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	testCodexSessionFailoverSettingsRoundTrip(test, db)
}

func testCodexSessionFailoverSettingsRoundTrip(test *testing.T, db *DB) {
	test.Helper()
	ctx := context.Background()
	for _, enabled := range []bool{true, false} {
		for _, preservePatterns := range []bool{true, false} {
			for _, preserveKey := range []bool{true, false} {
				test.Run(fmt.Sprintf("enabled=%t/patterns=%t/key=%t", enabled, preservePatterns, preserveKey), func(test *testing.T) {
					settings := &SystemSettings{
						CodexSessionFailoverEnabled: !enabled,
						CodexImagesMainModel:        "gpt-5.6-luna",
						CodexTelemetryEnabled:       true,
						PromptFilterCustomPatterns:  "[]",
						PromptFilterReviewAPIKey:    "original-key",
					}
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					settings.CodexSessionFailoverEnabled = enabled
					settings.PromptFilterCustomPatterns = `[{"id":"new","pattern":"new"}]`
					settings.PromptFilterReviewAPIKey = "new-key"
					settings.PreservePromptFilterCustomPatterns = preservePatterns
					settings.PreservePromptFilterReviewAPIKey = preserveKey
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					persisted, err := db.GetSystemSettings(ctx)
					if err != nil || persisted == nil {
						test.Fatalf("read updated settings: %v", err)
					}
					if persisted.CodexSessionFailoverEnabled != enabled || persisted.CodexImagesMainModel != settings.CodexImagesMainModel || !persisted.CodexTelemetryEnabled {
						test.Fatal("failover did not round trip independently of adjacent SQL parameters")
					}
					wantPatterns, wantKey := settings.PromptFilterCustomPatterns, settings.PromptFilterReviewAPIKey
					if preservePatterns {
						wantPatterns = "[]"
					}
					if preserveKey {
						wantKey = "original-key"
					}
					if persisted.PromptFilterCustomPatterns != wantPatterns || persisted.PromptFilterReviewAPIKey != wantKey {
						test.Fatal("prompt filter preservation guards no longer match their SQL parameters")
					}
				})
			}
		}
	}
}
