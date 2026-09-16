package database

func NormalizeCodexInitialSessionMaxAgeSeconds(value int) int {
	if value < 1 || value > 86400 {
		return 60
	}
	return value
}
