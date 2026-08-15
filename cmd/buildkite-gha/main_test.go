package main

import "testing"

func TestDevelopmentVersionIncludesRevision(t *testing.T) {
	for _, test := range []struct {
		name     string
		version  string
		revision string
		want     string
	}{
		{name: "release", version: "0.13.11", revision: "0123456789abcdef0123456789abcdef01234567", want: "0.13.11"},
		{name: "development revision", version: "dev", revision: "0123456789abcdef0123456789abcdef01234567", want: "dev+0123456789ab"},
		{name: "missing revision", version: "dev", want: "dev"},
		{name: "invalid revision", version: "dev", revision: "not-a-commit", want: "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := developmentVersion(test.version, test.revision); got != test.want {
				t.Fatalf("developmentVersion() = %q, want %q", got, test.want)
			}
		})
	}
}
