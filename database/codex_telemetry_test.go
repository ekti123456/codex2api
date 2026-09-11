package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLiteCodexTelemetrySettingRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexTelemetryEnabled {
		t.Fatalf("default telemetry setting must be off: %#v, err = %v", settings, err)
	}
	settings.CodexTelemetryEnabled = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("enable telemetry: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || !settings.CodexTelemetryEnabled {
		t.Fatalf("persisted telemetry setting = %#v, err = %v", settings, err)
	}
	settings.CodexTelemetryEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("disable telemetry: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexTelemetryEnabled {
		t.Fatalf("persisted telemetry setting = %#v, err = %v", settings, err)
	}
}

func TestSQLiteCodexTelemetryMigrationDefaultsOff(test *testing.T) {
	path := filepath.Join(test.TempDir(), "legacy-telemetry.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	_, err = db.conn.ExecContext(context.Background(), `INSERT INTO system_settings (id) VALUES (1)`)
	require.NoError(test, err)
	require.NoError(test, db.Close())
	legacy, err := sql.Open("sqlite", path)
	require.NoError(test, err)
	_, err = legacy.Exec(`ALTER TABLE system_settings DROP COLUMN codex_telemetry_enabled`)
	require.NoError(test, err)
	require.NoError(test, legacy.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { _ = db.Close() })
	settings, err := db.GetSystemSettings(context.Background())
	require.NoError(test, err)
	require.False(test, settings.CodexTelemetryEnabled)
}
