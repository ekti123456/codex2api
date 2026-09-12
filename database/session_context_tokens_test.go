package database

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionContextTokensPersistAndExpire(test *testing.T) {
	path := filepath.Join(test.TempDir(), "context.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	valid, expired := strings.Repeat("a", 64), strings.Repeat("b", 64)
	require.NoError(test, db.RecordSessionContextTokens(test.Context(), map[string]time.Time{valid: time.Now().Add(time.Hour), expired: time.Now().Add(-time.Second)}))
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	for _, key := range []string{valid, expired, strings.Repeat("c", 64)} {
		found, err := db.HasSessionContextToken(test.Context(), key)
		require.NoError(test, err)
		require.Equal(test, key == valid, found)
	}
	require.NoError(test, db.RecordSessionContextTokens(test.Context(), map[string]time.Time{valid: time.Now().Add(-time.Second)}))
	found, err := db.HasSessionContextToken(test.Context(), valid)
	require.NoError(test, err)
	require.True(test, found)
	require.Error(test, db.RecordSessionContextTokens(test.Context(), map[string]time.Time{"raw-content": time.Now().Add(time.Hour)}))
}
