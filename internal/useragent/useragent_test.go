package useragent

import (
	"strings"
	"testing"
)

func TestFromVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		want    string
	}{
		{version: "1.2.3", want: "buildkite-gha/1.2.3"},
		{version: "dev+0123456789ab", want: "buildkite-gha/dev+0123456789ab"},
		{want: "buildkite-gha/unknown"},
		{version: "unsafe\r\nHeader", want: "buildkite-gha/unknown"},
		{version: strings.Repeat("v", maxVersionBytes+1), want: "buildkite-gha/unknown"},
	} {
		if got := FromVersion(test.version); got != test.want {
			t.Errorf("FromVersion(%q) = %q, want %q", test.version, got, test.want)
		}
	}
}
