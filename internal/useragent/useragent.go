package useragent

const (
	product         = "buildkite-gha"
	maxVersionBytes = 64
)

// FromVersion returns the product token sent with HTTP requests.
func FromVersion(version string) string {
	if !validToken(version) {
		version = "unknown"
	}
	return product + "/" + version
}

func validToken(value string) bool {
	if value == "" || len(value) > maxVersionBytes {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '!' || character == '#' || character == '$' || character == '%' || character == '&' ||
			character == '\'' || character == '*' || character == '+' || character == '-' || character == '.' ||
			character == '^' || character == '_' || character == '`' || character == '|' || character == '~' {
			continue
		}
		return false
	}
	return true
}
