package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionOutboundWindowsPersistAndSeparateGenerations(test *testing.T) {
	path := filepath.Join(test.TempDir(), "windows.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	ctx := context.Background()
	created := time.Now().Add(-time.Hour).UTC()
	_, err = db.CommitSessionContinuity(ctx, "root", SessionContinuityRecord{AccountID: 1, ThreadID: "main", Number: 47, NumberKnown: true, LastSeen: created})
	require.NoError(test, err)
	_, _, err = db.SwitchSessionContinuityAccount(ctx, SessionAccountFailover{RootKey: "root", ExpectedAccountID: 1, AccountID: 2, At: time.Now(), ResetOutboundWindow: true, WindowThreadID: "main", WindowNumber: 47})
	require.NoError(test, err)
	bases, err := db.ResolveSessionOutboundWindows(ctx, "root", 2, 1, map[string]uint64{"main": 48, "child": 3})
	require.NoError(test, err)
	require.EqualValues(test, 47, bases["main"])
	require.EqualValues(test, 3, bases["child"])
	_, err = db.ResolveSessionOutboundWindows(ctx, "root", 2, 1, map[string]uint64{"child": 2})
	require.Error(test, err)
	_, err = db.ResolveSessionOutboundWindows(ctx, "root", 1, 1, map[string]uint64{"main": 48})
	require.Error(test, err)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	bases, err = db.ResolveSessionOutboundWindows(ctx, "root", 2, 1, map[string]uint64{"main": 49, "child": 4})
	require.NoError(test, err)
	require.EqualValues(test, 47, bases["main"])
	require.EqualValues(test, 3, bases["child"])
	record, _, err := db.SwitchSessionContinuityAccount(ctx, SessionAccountFailover{RootKey: "root", ExpectedAccountID: 2, AccountID: 1, ExpectedGeneration: 1, At: time.Now(), ResetOutboundWindow: true, WindowThreadID: "main", WindowNumber: 49})
	require.NoError(test, err)
	require.Equal(test, map[string]uint64{"main": 49}, record.OutboundWindowBases)
	require.True(test, created.Equal(record.LastSeen))
	_, err = db.ResolveSessionOutboundWindows(ctx, "root", 2, 1, map[string]uint64{"child": 4})
	require.Error(test, err)
	record, err = db.CommitSessionContinuity(ctx, "root", SessionContinuityRecord{AccountID: 1, ThreadID: "main", Number: 50, NumberKnown: true, FailoverCount: record.FailoverCount})
	require.NoError(test, err)
	require.True(test, record.OutboundWindowReset)
	require.EqualValues(test, 49, record.OutboundWindowBases["main"])
}
