package auth

import (
	"slices"
	"strings"
)

func normalizedAccountTags(tags []string) []string {
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			result = append(result, tag)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func (account *Account) TagSnapshot() []string {
	account.mu.RLock()
	defer account.mu.RUnlock()
	return slices.Clone(account.Tags)
}

func (account *Account) HasExactTags(tags []string) bool {
	return account != nil && slices.Equal(normalizedAccountTags(account.TagSnapshot()), normalizedAccountTags(tags))
}
