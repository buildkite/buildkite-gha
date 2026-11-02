package runtime

import (
	"bytes"
	"testing"
)

func TestQualifyHostMapping(t *testing.T) {
	for _, test := range []struct {
		name, hosts, want string
		wantError         bool
	}{
		{
			name:  "both address families retain whitespace comments and aliases",
			hosts: "# probe\n127.0.0.1 localhost\n 192.0.2.4\tother probe alias # comment\r\n2001:db8::4 probe probe.old.internal",
			want:  "# probe\n127.0.0.1 localhost\n 192.0.2.4\tprobe.buildkite.internal other probe alias # comment\r\n2001:db8::4 probe.buildkite.internal probe probe.old.internal",
		},
		{
			name:  "custom canonical name prevents overriding either address family",
			hosts: "192.0.2.4 probe\n2001:db8::4 custom.internal probe\n",
			want:  "192.0.2.4 probe\n2001:db8::4 custom.internal probe\n",
		},
		{
			name:      "no usable self address",
			hosts:     "127.0.0.1 localhost probe\n0.0.0.0 probe\nnot-an-ip probe\n192.0.2.4 other # probe\n",
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := qualifyHostMapping([]byte(test.hosts), "probe")
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%v", err, test.wantError)
			}
			if test.wantError {
				return
			}
			if string(got) != test.want {
				t.Fatalf("hosts=%q, want %q", got, test.want)
			}
			again, err := qualifyHostMapping(got, "probe")
			if err != nil || !bytes.Equal(got, again) {
				t.Fatalf("second correction changed hosts: %q, %v", again, err)
			}
		})
	}
}
