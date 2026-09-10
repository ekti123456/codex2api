package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExpansionReservationRollsBackAndDoesNotHoldGlobalAdmissionLock(test *testing.T) {
	store, owner, other := newHardWindowFallbackTestStore()
	owner.SessionCapacityMax, owner.SessionCapacityReserved = 2, 1
	other.SessionCapacityEnabled, other.SessionCapacityMax = true, 2
	require.True(test, store.AdmitAccountSession(owner, "ordinary", time.Now()))
	failed := errors.New("transaction failed")
	err := store.WithExpandedSessionReservation(owner.ID(), "upgrade", func() error {
		require.True(test, store.AdmitAccountSession(other, "independent", time.Now()))
		require.False(test, store.AdmitAccountSession(owner, "new", time.Now()))
		return failed
	})
	require.ErrorIs(test, err, failed)
	total, reserved := store.AccountSessionSlotCounts(owner.ID(), time.Now())
	require.EqualValues(test, 1, total)
	require.Zero(test, reserved)
	require.NoError(test, store.WithExpandedSessionReservation(owner.ID(), "upgrade", func() error { return nil }))
	require.ErrorIs(test, store.WithExpandedSessionReservation(owner.ID(), "third", func() error {
		test.Fatal("full reserved slots must never commit pricing")
		return nil
	}), ErrReservedSessionFull)
}
