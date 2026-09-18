package database

func cloneUsageTurnStateInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
