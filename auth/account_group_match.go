package auth

import "slices"

func (account *Account) HasExactGroupIDs(groupIDs []int64) bool {
	if account == nil {
		return false
	}
	return slices.Equal(normalizeAllowedGroupIDs(account.GroupIDSnapshot()), normalizeAllowedGroupIDs(groupIDs))
}
