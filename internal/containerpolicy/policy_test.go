package containerpolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestJobOptionsAcceptedSyntax(t *testing.T) {
	value := `--privileged --user 1000 --label "description=two words" --mount type=tmpfs,dst=/tmp --name=custom`
	want := []string{"--privileged", "--user", "1000", "--label", "description=two words", "--mount", "type=tmpfs,dst=/tmp", "--name=custom"}
	got, err := JobOptions(value)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("JobOptions(%q) = %#v, %v; want %#v", value, got, err, want)
	}
}

func TestJobOptionsRejectsUnsupportedOverrides(t *testing.T) {
	for _, value := range []string{
		"--network host", "--network=host", "--net host", "--net=host",
		"--entrypoint sh", "--entrypoint=sh",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := JobOptions(value); err == nil {
				t.Fatal("JobOptions() accepted an unsupported override")
			}
		})
	}
}

func TestJobOptionsInputBounds(t *testing.T) {
	if _, err := JobOptions(strings.Repeat("x", MaxJobOptionsLength)); err != nil {
		t.Fatalf("JobOptions() rejected the maximum input length: %v", err)
	}
	for _, value := range []string{strings.Repeat("x", MaxJobOptionsLength+1), "--label=a\n--privileged", "--label=a\x00b"} {
		if _, err := JobOptions(value); err == nil {
			t.Fatal("JobOptions() accepted out-of-bounds input")
		}
	}
}

func TestValidateJobVolume(t *testing.T) {
	for _, value := range []string{
		"v:/data", "cache:/cache", "cache.v1:/var/cache:ro", "CACHE_1:/data:rw",
		"/anonymous", "/srv/cache:/cache", "/srv/cache:/cache:rw",
	} {
		if err := ValidateJobVolume(value); err != nil {
			t.Errorf("ValidateJobVolume(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"-bad:/data", "relative/path:/data", "cache:data", "cache:/data:z", ":/data", "/anonymous:ro"} {
		if err := ValidateJobVolume(value); err == nil {
			t.Errorf("ValidateJobVolume(%q) accepted invalid volume", value)
		}
	}
	if err := ValidateJobVolumes([]string{"cache:/one", "cache:/one"}); err == nil {
		t.Error("ValidateJobVolumes() accepted a repeated volume")
	}
	if err := ValidateJobVolumes([]string{"one:/cache", "two:/cache"}); err != nil {
		t.Errorf("ValidateJobVolumes() rejected Docker-owned duplicate target validation: %v", err)
	}
}
