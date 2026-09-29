//go:build !windows

package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	// nsc-remote as observed on a native Namespace runner: TLS driver options
	// as paths, one pinned platform, no embedded files.
	nscRemoteBuilder = `{"Name":"nsc-remote","Driver":"remote","Nodes":[{"Name":"amd64","Endpoint":"tcp://amd64.build.iad5.namespaceapis.com:443","Platforms":[{"architecture":"amd64","os":"linux"}],"DriverOpts":{"cacert":"/home/runner/.config/ns/buildkit/proxy/server_amd64_cert.pem","cert":"/var/run/secrets/guest/public.pem","key":"/var/run/secrets/guest/private.key"},"Flags":null,"Files":null}],"Dynamic":false}`
	// A stored builder for the docker driver, which always loads its result.
	dockerDriverBuilder = `{"Name":"other","Driver":"docker","Nodes":[{"Name":"other","Endpoint":"unix:///var/run/docker.sock","Platforms":null,"DriverOpts":null,"Flags":null,"Files":null}],"Dynamic":false}`
	// in-runner-builder: no driver options, two pinned platforms.
	inRunnerBuilder = `{"Name":"in-runner-builder","Driver":"remote","Nodes":[{"Name":"in-runner-builder0","Endpoint":"unix:///var/run/buildkit/buildkitd.sock","Platforms":[{"architecture":"amd64","os":"linux"},{"architecture":"arm64","os":"linux"}],"DriverOpts":null,"Flags":null,"Files":null}],"Dynamic":false}`
	// nsc-remote on a tenant with two nodes, one per architecture.
	twoNodeRemoteBuilder = `{"Name":"nsc-remote","Driver":"remote","Nodes":[{"Name":"amd64","Endpoint":"tcp://amd64.build.iad2-b.namespaceapis.com:443","Platforms":[{"architecture":"amd64","os":"linux"}],"DriverOpts":{"cacert":"/a","cert":"/b","key":"/c"},"Flags":null,"Files":null},{"Name":"arm64","Endpoint":"tcp://arm64.build.iad2-b.namespaceapis.com:443","Platforms":[{"architecture":"arm64","os":"linux"}],"DriverOpts":{"cacert":"/d","cert":"/b","key":"/c"},"Flags":null,"Files":null}],"Dynamic":false}`
)

func TestWithBuildxDefaultLoad(t *testing.T) {
	for _, test := range []struct {
		name, stored, want string
		changed            bool
	}{
		{
			name:    "remote builder with TLS options keeps every option and pinned platform",
			stored:  nscRemoteBuilder,
			want:    `{"Name":"nsc-remote","Driver":"remote","Nodes":[{"Name":"amd64","Endpoint":"tcp://amd64.build.iad5.namespaceapis.com:443","Platforms":[{"architecture":"amd64","os":"linux"}],"DriverOpts":{"cacert":"/home/runner/.config/ns/buildkit/proxy/server_amd64_cert.pem","cert":"/var/run/secrets/guest/public.pem","default-load":"true","key":"/var/run/secrets/guest/private.key"},"Flags":null,"Files":null}],"Dynamic":false}`,
			changed: true,
		},
		{
			name:    "remote builder without options gains the option and keeps both platforms",
			stored:  inRunnerBuilder,
			want:    `{"Name":"in-runner-builder","Driver":"remote","Nodes":[{"Name":"in-runner-builder0","Endpoint":"unix:///var/run/buildkit/buildkitd.sock","Platforms":[{"architecture":"amd64","os":"linux"},{"architecture":"arm64","os":"linux"}],"DriverOpts":{"default-load":"true"},"Flags":null,"Files":null}],"Dynamic":false}`,
			changed: true,
		},
		{
			name:    "existing options, flags, files, and unknown fields survive",
			stored:  `{"Name":"one","Driver":"docker-container","Nodes":[{"Name":"b","Endpoint":"ssh://arm64","Platforms":[{"architecture":"arm64","os":"linux","variant":"v8"}],"DriverOpts":{"network":"host"},"Flags":["--debug"],"Files":{"buildkitd.toml":"W3dvcmtlci5vY2ld"}}],"Dynamic":true,"Future":{"n":1.50}}`,
			want:    `{"Name":"one","Driver":"docker-container","Nodes":[{"Name":"b","Endpoint":"ssh://arm64","Platforms":[{"architecture":"arm64","os":"linux","variant":"v8"}],"DriverOpts":{"default-load":"true","network":"host"},"Flags":["--debug"],"Files":{"buildkitd.toml":"W3dvcmtlci5vY2ld"}}],"Dynamic":true,"Future":{"n":1.50}}`,
			changed: true,
		},
		{
			name:   "builder that already loads is unchanged",
			stored: `{"Name":"nsc-remote","Driver":"remote","Nodes":[{"Name":"amd64","Endpoint":"tcp://x:443","Platforms":null,"DriverOpts":{"default-load":"true"},"Flags":null,"Files":null}],"Dynamic":false}`,
		},
		{
			name:   "docker driver never receives the option",
			stored: `{"Name":"desktop-linux","Driver":"docker","Nodes":[{"Name":"desktop-linux","Endpoint":"desktop-linux","Platforms":null,"DriverOpts":null,"Flags":null,"Files":null}],"Dynamic":false}`,
		},
		{
			name:   "unknown driver is left alone because it may reject the option",
			stored: `{"Name":"x","Driver":"future","Nodes":[{"Name":"x0","Endpoint":"tcp://x","Platforms":null,"DriverOpts":null,"Flags":null,"Files":null}],"Dynamic":false}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated, changed, err := withBuildxDefaultLoad([]byte(test.stored))
			if err != nil || changed != test.changed {
				t.Fatalf("withBuildxDefaultLoad() changed = %v, error = %v, want changed %v", changed, err, test.changed)
			}
			if !test.changed {
				if updated != nil {
					t.Fatalf("unchanged builder returned data %s", updated)
				}
				return
			}
			var got, want any
			if err := json.Unmarshal(updated, &got); err != nil {
				t.Fatalf("decode updated builder: %v\n%s", err, updated)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("updated builder = %s\nwant %s", updated, test.want)
			}
			if !bytes.Contains(updated, []byte(`"n":1.50`)) && strings.Contains(test.want, `1.50`) {
				t.Fatalf("numeric literal was not preserved: %s", updated)
			}
		})
	}
	for _, stored := range []string{"not json", `{"Driver":"remote"}`, `{"Driver":"remote","Nodes":["x"]}`, `{"Driver":"remote","Nodes":[{"DriverOpts":"cacert=x"}]}`} {
		if _, changed, err := withBuildxDefaultLoad([]byte(stored)); err == nil || changed || errors.Is(err, errBuildxMultiNode) {
			t.Fatalf("withBuildxDefaultLoad(%q) changed = %v, error = %v, want error", stored, changed, err)
		}
	}
	// A builder with more than one node is reported, not changed: buildx
	// rejects the OCI export default-load implies when a build spans nodes.
	if updated, changed, err := withBuildxDefaultLoad([]byte(twoNodeRemoteBuilder)); !errors.Is(err, errBuildxMultiNode) || changed || updated != nil {
		t.Fatalf("withBuildxDefaultLoad(two nodes) = %s, %v, %v, want errBuildxMultiNode", updated, changed, err)
	}
}

func TestBuildxStoreDir(t *testing.T) {
	for _, test := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HOME": "/home/runner", "DOCKER_CONFIG": "/etc/docker-cli", "BUILDX_CONFIG": "/var/buildx"}, "/var/buildx"},
		{map[string]string{"HOME": "/home/runner", "DOCKER_CONFIG": "/etc/docker-cli"}, "/etc/docker-cli/buildx"},
		{map[string]string{"HOME": "/home/runner"}, "/home/runner/.docker/buildx"},
		{map[string]string{}, ""},
	} {
		if got := buildxStoreDir(test.env); got != test.want {
			t.Errorf("buildxStoreDir(%v) = %q, want %q", test.env, got, test.want)
		}
	}
}

func writeBuildxStore(t *testing.T, dir, name, stored string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "instances"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "instances", name)
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnableBuildxDefaultLoad(t *testing.T) {
	storeDir := t.TempDir()
	remote := writeBuildxStore(t, storeDir, "nsc-remote", nscRemoteBuilder)
	docker := writeBuildxStore(t, storeDir, "other", dockerDriverBuilder)
	loaded := `{"Name":"ready","Driver":"remote","Nodes":[{"Name":"ready0","Endpoint":"tcp://x:443","Platforms":null,"DriverOpts":{"default-load":"true"},"Flags":null,"Files":null}],"Dynamic":false}`
	ready := writeBuildxStore(t, storeDir, "ready", loaded)
	twoNode := writeBuildxStore(t, storeDir, "two-node", twoNodeRemoteBuilder)
	if err := os.Mkdir(filepath.Join(storeDir, "instances", "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	enabled, multiNode, err := enableBuildxDefaultLoad(filepath.Join(storeDir, "instances"))
	if err != nil || !reflect.DeepEqual(enabled, []string{"nsc-remote"}) || !reflect.DeepEqual(multiNode, []string{"two-node"}) {
		t.Fatalf("enableBuildxDefaultLoad() = %q, %q, %v, want [nsc-remote], [two-node]", enabled, multiNode, err)
	}
	updated, err := os.ReadFile(remote)
	if err != nil || !strings.Contains(string(updated), `"default-load":"true"`) || !strings.Contains(string(updated), `"cacert":"/home/runner/.config/ns/buildkit/proxy/server_amd64_cert.pem"`) {
		t.Fatalf("nsc-remote = %s, %v", updated, err)
	}
	for path, want := range map[string]string{docker: dockerDriverBuilder, ready: loaded, twoNode: twoNodeRemoteBuilder} {
		if stored, err := os.ReadFile(path); err != nil || string(stored) != want {
			t.Fatalf("%s = %s, %v, want unchanged", path, stored, err)
		}
	}
	// A second pass finds nothing to change and reports the same skip.
	if enabled, multiNode, err := enableBuildxDefaultLoad(filepath.Join(storeDir, "instances")); err != nil || len(enabled) != 0 || !reflect.DeepEqual(multiNode, []string{"two-node"}) {
		t.Fatalf("second enableBuildxDefaultLoad() = %q, %q, %v", enabled, multiNode, err)
	}
	if enabled, multiNode, err := enableBuildxDefaultLoad(filepath.Join(t.TempDir(), "instances")); err != nil || enabled != nil || multiNode != nil {
		t.Fatalf("enableBuildxDefaultLoad(missing) = %q, %q, %v, want nil, nil, nil", enabled, multiNode, err)
	}
	writeBuildxStore(t, storeDir, "broken", "not json")
	if _, _, err := enableBuildxDefaultLoad(filepath.Join(storeDir, "instances")); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("enableBuildxDefaultLoad(broken) error = %v, want decode error naming the file", err)
	}
}

func TestRunJobEnablesDockerBuildLoadBeforeSteps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeBuildxStore(t, filepath.Join(home, ".docker", "buildx"), "in-runner-builder", inRunnerBuilder)
	writeBuildxStore(t, filepath.Join(home, ".docker", "buildx"), "nsc-remote", twoNodeRemoteBuilder)
	workspace := t.TempDir()
	workflow := ".github/workflows/build.yml"
	writeFixtureFile(t, workspace, workflow, "name: build\n")
	job := runtimePlan(t, workspace, workflow, []runtimeTestStep{{ID: "build", Kind: "run", Command: `grep -q '"default-load":"true"' "$HOME/.docker/buildx/instances/in-runner-builder" && ! grep -q default-load "$HOME/.docker/buildx/instances/nsc-remote"`}})
	var stdout, stderr bytes.Buffer
	result, err := (Runner{Stdout: &stdout, Stderr: &stderr, DockerBuildLoad: true}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() = %#v, %v\nstdout: %s\nstderr: %s", result, err, stdout.String(), stderr.String())
	}
	want := "buildkite-gha: enabled default-load on buildx builder in-runner-builder so docker build loads images into the Docker daemon\n" +
		"buildkite-gha: buildx builder nsc-remote has more than one node, so plain docker build will not load images into the Docker daemon there; use --load\n"
	if !strings.HasPrefix(stdout.String(), want) || stderr.Len() != 0 {
		t.Fatalf("stdout = %q\nstderr = %q", stdout.String(), stderr.String())
	}
}
