package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const liveProbePath = ".buildkite/phase-0-transport-probe/probe.sh"

func TestLiveProbeUsesOnlyPinnedRepositoryCommands(t *testing.T) {
	root := filepath.Join("..", "..", ".buildkite", "phase-0-transport-probe")
	pipeline := readProbeFile(t, filepath.Join(root, "pipeline.yml"))
	if strings.Contains(pipeline, "checkout:\n      skip: true") {
		t.Fatal("unprivileged live probe must use the exact build checkout")
	}
	commands := regexp.MustCompile(`(?m)^    command: "([^"]+)"$`).FindAllStringSubmatch(pipeline, -1)
	if len(commands) != 2 {
		t.Fatalf("static probe commands = %d, want 2", len(commands))
	}
	for _, command := range commands {
		if !strings.HasPrefix(command[1], liveProbePath+" ") {
			t.Fatalf("live probe command is not repository-pinned: %q", command[1])
		}
	}

	script := readProbeFile(t, filepath.Join(root, "probe.sh"))
	if !strings.Contains(script, `readonly probe_path="`+liveProbePath+`"`) || strings.Count(script, `command: "${probe_path} `) != 2 {
		t.Fatal("generated jobs must use the exact build checkout")
	}
	for _, required := range []string{
		`readonly binding_issuer="buildkite-gha-plan-envelope"`,
		`PHASE0_BINDING_JTI`,
		`PHASE0_REDACTION_SECRET`,
		`PHASE0_PRODUCER_PLAN_DIGEST`,
		`cmp -s "${canonical}" "${manifest}"`,
		`.producer.job_id == $job_id`,
		`artifact search "buildkite-gha/v1/results/${producer_key}/manifest.json" --step "${producer_key}" --format '%j'`,
		`.plan_digest == $plan_digest`,
		`.result == $result`,
		`(.exp - .iat) <= $max_lifetime`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("probe is missing trust or result verification %q", required)
		}
	}
}

func TestProbeAndGoUseTheSameJCSRuntimeBinding(t *testing.T) {
	root := filepath.Join("..", "..", ".buildkite", "phase-0-transport-probe")
	fixture := []byte(readProbeFile(t, filepath.Join(root, "fixtures", "runtime-binding.claims.json")))
	fixture = bytes.TrimSuffix(fixture, []byte("\n"))

	var binding RuntimeBinding
	if err := json.Unmarshal(fixture, &binding); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, fixture) {
		t.Fatalf("Go JCS differs from fixture\nwant: %s\ngot:  %s", fixture, canonical)
	}

	key := testKey(t)
	encoded, err := SignRuntimeBinding(key, binding)
	if err != nil {
		t.Fatal(err)
	}
	var signed signedValue
	if err := json.Unmarshal([]byte(encoded), &signed); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(signed.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, fixture) {
		t.Fatalf("signed Go payload differs from fixture\nwant: %s\ngot:  %s", fixture, payload)
	}

	command := exec.Command("bash", filepath.Join(root, "probe.sh"), "canonicalize")
	command.Stdin = bytes.NewReader(fixture)
	probeCanonical, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	probeCanonical = bytes.TrimSuffix(probeCanonical, []byte("\n"))
	if !bytes.Equal(probeCanonical, fixture) {
		t.Fatalf("probe JCS differs from fixture\nwant: %s\ngot:  %s", fixture, probeCanonical)
	}
}

func readProbeFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
