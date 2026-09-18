package userfacing

import "strings"

// ZHEN formats a user-visible API message as Chinese then English, matching
// the existing cyber-session style: "中文 / English".
func ZHEN(zh, en string) string {
	zh = strings.TrimSpace(zh)
	en = strings.TrimSpace(en)
	switch {
	case zh == "" && en == "":
		return ""
	case zh == "":
		return en
	case en == "":
		return zh
	case zh == en:
		return zh
	default:
		return zh + " / " + en
	}
}
