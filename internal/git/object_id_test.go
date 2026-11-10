package git

import (
	"strings"
	"testing"
)

func TestValidObjectID(t *testing.T) {
	t.Parallel()
	valid := "0123456789abcdef0123456789abcdef01234567"
	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "lowercase SHA-1", value: valid, want: true},
		{name: "empty"},
		{name: "short", value: valid[:39]},
		{name: "long", value: valid + "0"},
		{name: "uppercase", value: strings.ToUpper(valid)},
		{name: "non-hex", value: strings.Repeat("g", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidObjectID(test.value); got != test.want {
				t.Fatalf("ValidObjectID(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}
