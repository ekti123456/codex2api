package auth

import (
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/timezone"
)

func buildProxyLocations(rows []*database.ProxyRow) map[string]database.ProxyLocation {
	locations := make(map[string]database.ProxyLocation, len(rows))
	prefer := func(manual, detected string) string {
		if strings.TrimSpace(manual) != "" {
			return manual
		}
		return detected
	}
	for _, row := range rows {
		if row == nil || strings.TrimSpace(row.URL) == "" {
			continue
		}
		locations[strings.TrimSpace(row.URL)] = database.ProxyLocation{
			Country:  database.NormalizeProxyCountryCode(prefer(row.CountryCodeOverride, row.TestCountryCode)),
			Region:   database.NormalizeProxyLocationText(prefer(row.RegionOverride, row.TestRegion)),
			City:     database.NormalizeProxyLocationText(prefer(row.CityOverride, row.TestCity)),
			Timezone: timezone.Normalize(prefer(row.TimezoneOverride, row.TestTimezone)),
		}
	}
	return locations
}

// ProxyLocation returns a value snapshot; inference never queries a geo service.
func (store *Store) ProxyLocation(proxyURL string) database.ProxyLocation {
	if store == nil {
		return database.ProxyLocation{}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.proxyLocations[strings.TrimSpace(proxyURL)]
}
