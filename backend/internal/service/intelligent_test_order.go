package service

import "sort"

// Admin console order: tibo, candy, pelican, then any later types by name.
func IntelligentTestDisplayRank(testType string) int {
	switch testType {
	case "tibo":
		return 0
	case "candy":
		return 1
	case "pelican":
		return 2
	default:
		return 100
	}
}

func SortIntelligentTestSettings(settings []IntelligentTestSetting) {
	sort.SliceStable(settings, func(i, j int) bool {
		ri, rj := IntelligentTestDisplayRank(settings[i].TestType), IntelligentTestDisplayRank(settings[j].TestType)
		if ri != rj {
			return ri < rj
		}
		return settings[i].TestType < settings[j].TestType
	})
}
