package containerpolicy

import (
	"slices"
	"testing"
)

func TestJobOptionsAcceptedSyntax(t *testing.T) {
	value := "--cpus=.5 --cpuset-cpus 0-2,4 -m 512m --memory-reservation=256M --memory-swap 1g --pids-limit=128 --shm-size 64m"
	want := []string{"--cpus=.5", "--cpuset-cpus", "0-2,4", "-m", "512m", "--memory-reservation=256M", "--memory-swap", "1g", "--pids-limit=128", "--shm-size", "64m"}
	got, err := JobOptions(value)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("JobOptions(%q) = %#v, %v; want %#v", value, got, err, want)
	}
}

func TestJobOptionsRejectsInvalidSyntax(t *testing.T) {
	for _, value := range []string{
		"--cpus 0", "--cpus NaN", "--cpuset-cpus 3-1", "--memory -1", "--memory 1t",
		"--pids-limit 0", "--shm-size", "--cpus 1 --cpus=2", "--memory 1g extra",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := JobOptions(value); err == nil {
				t.Fatal("JobOptions() accepted invalid syntax")
			}
		})
	}
}

func TestValidateJobVolume(t *testing.T) {
	for _, value := range []string{"cache:/cache", "cache.v1:/var/cache:ro", "CACHE_1:/data:rw"} {
		if err := ValidateJobVolume(value); err != nil {
			t.Errorf("ValidateJobVolume(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"v:/data", "/tmp:/data", "cache:data", "cache:/", "cache:/data/../other", "cache:/__w", "cache:/__buildkite-gha/runtime", "cache:/data:z"} {
		if err := ValidateJobVolume(value); err == nil {
			t.Errorf("ValidateJobVolume(%q) accepted invalid volume", value)
		}
	}
	if err := ValidateJobVolumes([]string{"cache:/one", "cache:/one"}); err == nil {
		t.Error("ValidateJobVolumes() accepted a repeated volume")
	}
	if err := ValidateJobVolumes([]string{"one:/cache", "two:/cache"}); err == nil {
		t.Error("ValidateJobVolumes() accepted a repeated target")
	}
}
