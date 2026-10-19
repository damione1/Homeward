// Package mqtttopic contains the dependency-free admission grammar shared by
// the MQTT transport and the native integration edge. Matching belongs to mqtt.
package mqtttopic

import (
	"strings"
	"unicode/utf8"
)

// ValidBase admits the common topic/filter shape. A filter's wildcard
// positions are checked by mqtt.ParseTopicFilter, not here.
func ValidBase(raw string) bool {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) ||
		strings.ContainsAny(raw, "\x00\r\n") || strings.HasPrefix(raw, "$share/") {
		return false
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "" {
			return false
		}
	}
	return true
}

// ValidName admits a concrete publication topic, never a subscription filter.
func ValidName(raw string) bool {
	return ValidBase(raw) && !strings.ContainsAny(raw, "+#")
}
