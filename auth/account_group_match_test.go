package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountHasExactGroupIDs(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		actual   []int64
		expected []int64
		matches  bool
	}{
		{"same", []int64{1, 2}, []int64{1, 2}, true},
		{"reordered", []int64{2, 1}, []int64{1, 2}, true},
		{"duplicates", []int64{1, 2, 2}, []int64{2, 1, 1}, true},
		{"subset", []int64{1}, []int64{1, 2}, false},
		{"superset", []int64{1, 2, 3}, []int64{1, 2}, false},
		{"overlap", []int64{1, 3}, []int64{1, 2}, false},
		{"disjoint", []int64{3, 4}, []int64{1, 2}, false},
		{"ungrouped", nil, []int64{}, true},
		{"missing_groups", nil, []int64{1}, false},
		{"extra_groups", []int64{1}, nil, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			account := &Account{GroupIDs: scenario.actual}
			require.Equal(test, scenario.matches, account.HasExactGroupIDs(scenario.expected))
			require.Equal(test, scenario.actual, account.GroupIDs)
		})
	}
	var missing *Account
	require.False(test, missing.HasExactGroupIDs(nil))
}
