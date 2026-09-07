package git

// ValidObjectID reports whether value is a lowercase 40-hex SHA-1 Git object ID.
func ValidObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
