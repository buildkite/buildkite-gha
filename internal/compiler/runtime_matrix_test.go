package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/workflow"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestExpandRuntimeMatrixOutputAcceptsPinnedPostHogIncludeShape(t *testing.T) {
	descriptor := validRuntimeMatrixDescriptor(RuntimeMatrixShapeInclude)
	source := []byte(`[{
  "segment":"Core",
  "person-on-events":false,
  "new-events-schema":false,
  "python-version":"3.13.13",
  "clickhouse-server-image":"clickhouse/clickhouse-server:25.8",
  "concurrency":38,
  "group":1,
  "artifact_key":"core-1",
  "compat":false
}]`)
	matrices, err := ExpandRuntimeMatrixOutput(descriptor, source, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matrices) != 1 || matrices[0]["segment"] != "Core" || matrices[0]["group"] != json.Number("1") || matrices[0]["person-on-events"] != false {
		t.Fatalf("PostHog matrix = %#v", matrices)
	}

	empty, err := ExpandRuntimeMatrixOutput(descriptor, []byte(`[]`), MaxRuntimeMatrixGraphJobs)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty PostHog matrix = %#v, %v", empty, err)
	}
}

func TestExpandRuntimeMatrixOutputAcceptsBoundedMatrixObject(t *testing.T) {
	descriptor := validRuntimeMatrixDescriptor(RuntimeMatrixShapeObject)
	matrices, err := ExpandRuntimeMatrixOutput(descriptor, []byte(`{"os":["ubuntu"],"version":[1,2],"include":[{"os":"macos","version":3}],"exclude":[{"os":"ubuntu","version":2}]}`), 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"os": "ubuntu", "version": json.Number("1")},
		{"os": "macos", "version": json.Number("3")},
	}
	if !reflect.DeepEqual(matrices, want) {
		t.Fatalf("matrix object = %#v, want %#v", matrices, want)
	}
}

func TestExpandRuntimeMatrixOutputRejectsMalformedAndTypeConfusedValues(t *testing.T) {
	include := validRuntimeMatrixDescriptor(RuntimeMatrixShapeInclude)
	object := validRuntimeMatrixDescriptor(RuntimeMatrixShapeObject)
	tests := []struct {
		name       string
		descriptor RuntimeMatrixDescriptor
		source     string
		want       string
	}{
		{name: "malformed", descriptor: include, source: `[`, want: "EOF"},
		{name: "trailing", descriptor: include, source: `[] true`, want: "multiple JSON values"},
		{name: "null root", descriptor: include, source: `null`, want: "want array"},
		{name: "wrong include type", descriptor: include, source: `{}`, want: "want array"},
		{name: "non-object include", descriptor: include, source: `["job"]`, want: "want object"},
		{name: "null scalar", descriptor: include, source: `[{"os":null}]`, want: "null values are unsupported"},
		{name: "nested object injection", descriptor: include, source: `[{"permissions":{"contents":"write"}}]`, want: "nested map[string]interface {} values are unsupported"},
		{name: "nested array injection", descriptor: include, source: `[{"steps":[{"run":"pwn"}]}]`, want: "nested []interface {} values are unsupported"},
		{name: "invalid key", descriptor: include, source: `[{"runs on":"privileged"}]`, want: "invalid runtime matrix key"},
		{name: "duplicate key", descriptor: include, source: `[{"os":"one","os":"two"}]`, want: "duplicate JSON key"},
		{name: "ambiguous key", descriptor: include, source: `[{"os":"one","OS":"two"}]`, want: "ambiguous properties"},
		{name: "duplicate instance", descriptor: include, source: `[{"os":"one"},{"os":"one"}]`, want: "duplicate instances"},
		{name: "empty entry", descriptor: include, source: `[{}]`, want: "between 1 and"},
		{name: "wrong matrix root", descriptor: object, source: `[]`, want: "want object"},
		{name: "empty matrix object", descriptor: object, source: `{}`, want: "between 1 and"},
		{name: "dimension scalar", descriptor: object, source: `{"os":"ubuntu"}`, want: "want array"},
		{name: "dimension nested", descriptor: object, source: `{"os":[{"name":"ubuntu"}]}`, want: "nested map[string]interface {} values are unsupported"},
		{name: "empty dimension", descriptor: object, source: `{"os":[]}`, want: "between 1 and"},
		{name: "reserved case", descriptor: object, source: `{"Include":[]}`, want: "must be lowercase"},
		{name: "include dimension case ambiguity", descriptor: object, source: `{"os":["linux"],"include":[{"OS":"macos"}]}`, want: "ambiguous with dimension"},
		{name: "exclude dimension case ambiguity", descriptor: object, source: `{"os":["linux"],"exclude":[{"OS":"linux"}]}`, want: "ambiguous with dimension"},
		{name: "instance key case ambiguity", descriptor: include, source: `[{"os":"linux"},{"OS":"macos"}]`, want: "ambiguous property spellings"},
		{name: "huge number", descriptor: include, source: `[{"group":1e9999}]`, want: "finite range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ExpandRuntimeMatrixOutput(test.descriptor, []byte(test.source), 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ExpandRuntimeMatrixOutput() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExpandRuntimeMatrixOutputEnforcesByteCardinalityAndGraphBounds(t *testing.T) {
	descriptor := validRuntimeMatrixDescriptor(RuntimeMatrixShapeInclude)
	oversized := bytes.Repeat([]byte{' '}, MaxRuntimeMatrixBytes+1)
	if _, err := ExpandRuntimeMatrixOutput(descriptor, oversized, 0); err == nil || !strings.Contains(err.Error(), "maximum is 65536") {
		t.Fatalf("oversized output error = %v", err)
	}
	boundary := append([]byte(`[]`), bytes.Repeat([]byte{' '}, MaxRuntimeMatrixBytes-2)...)
	if matrices, err := ExpandRuntimeMatrixOutput(descriptor, boundary, MaxRuntimeMatrixGraphJobs); err != nil || len(matrices) != 0 {
		t.Fatalf("exact byte-boundary matrix = %#v, %v", matrices, err)
	}
	if _, err := ExpandRuntimeMatrixOutput(descriptor, []byte{'[', 0xff, ']'}, 0); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("invalid UTF-8 matrix error = %v", err)
	}

	rows := make([]map[string]any, MaxRuntimeMatrixInstances)
	for i := range rows {
		rows[i] = map[string]any{"group": i + 1}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	matrices, err := ExpandRuntimeMatrixOutput(descriptor, encoded, MaxRuntimeMatrixGraphJobs-MaxRuntimeMatrixInstances)
	if err != nil || len(matrices) != MaxRuntimeMatrixInstances {
		t.Fatalf("256-instance matrix = %d, %v", len(matrices), err)
	}

	rows = append(rows, map[string]any{"group": MaxRuntimeMatrixInstances + 1})
	encoded, err = json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRuntimeMatrixOutput(descriptor, encoded, 0); err == nil || !strings.Contains(err.Error(), "more than 256") {
		t.Fatalf("257-instance matrix error = %v", err)
	}

	one, err := json.Marshal(rows[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRuntimeMatrixOutput(descriptor, one, MaxRuntimeMatrixGraphJobs); err == nil || !strings.Contains(err.Error(), "graph beyond 1024 jobs") {
		t.Fatalf("graph limit error = %v", err)
	}

	objectDescriptor := validRuntimeMatrixDescriptor(RuntimeMatrixShapeObject)
	left := make([]int, 17)
	right := make([]int, 16)
	product, err := json.Marshal(map[string]any{"left": left, "right": right})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRuntimeMatrixOutput(objectDescriptor, product, 0); err == nil || !strings.Contains(err.Error(), "beyond 256 instances") {
		t.Fatalf("matrix product error = %v", err)
	}

	dimensions := make(map[string]any, MaxRuntimeMatrixProperties)
	for i := 0; i < MaxRuntimeMatrixProperties-1; i++ {
		dimensions[fmt.Sprintf("d%d", i)] = []any{i}
	}
	include := make(map[string]any, MaxRuntimeMatrixProperties)
	for i := 0; i < MaxRuntimeMatrixProperties; i++ {
		include[fmt.Sprintf("i%d", i)] = i
	}
	dimensions["include"] = []any{include}
	tooManyProperties, err := json.Marshal(dimensions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandRuntimeMatrixOutput(objectDescriptor, tooManyProperties, 0); err == nil || !strings.Contains(err.Error(), "more than 64 properties") {
		t.Fatalf("expanded property limit error = %v", err)
	}
}

func TestRuntimeMatrixDescriptorIsDeterministicStrictAndSchemaValid(t *testing.T) {
	descriptor := validRuntimeMatrixDescriptor(RuntimeMatrixShapeInclude)
	first, err := EncodeRuntimeMatrixDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeRuntimeMatrixDescriptor(descriptor)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("descriptor encoding is not deterministic: %v", err)
	}
	decoded, err := DecodeRuntimeMatrixDescriptor(first)
	if err != nil || !reflect.DeepEqual(decoded, descriptor) {
		t.Fatalf("descriptor round trip = %#v, %v", decoded, err)
	}

	for _, source := range [][]byte{
		bytes.Replace(first, []byte(`"schema":`), []byte(`"schema":"duplicate","schema":`), 1),
		bytes.Replace(first, []byte(`"job":`), []byte(`"unknown":true,"job":`), 1),
		append(append([]byte(nil), first...), []byte(`true`)...),
	} {
		if _, err := DecodeRuntimeMatrixDescriptor(source); err == nil {
			t.Fatalf("DecodeRuntimeMatrixDescriptor(%s) unexpectedly succeeded", source)
		}
	}

	changed := descriptor
	changed.MaxInstances--
	if _, err := EncodeRuntimeMatrixDescriptor(changed); err == nil || !strings.Contains(err.Error(), "immutable schema") {
		t.Fatalf("changed v1 limit error = %v", err)
	}
	for _, change := range []func(*RuntimeMatrixDescriptor){
		func(descriptor *RuntimeMatrixDescriptor) { descriptor.Job = strings.Repeat("a", 256) },
		func(descriptor *RuntimeMatrixDescriptor) { descriptor.Job = `caller\django` },
		func(descriptor *RuntimeMatrixDescriptor) { descriptor.SourcePath = string([]byte{'a', 0xff}) },
		func(descriptor *RuntimeMatrixDescriptor) { descriptor.Source.End.Column = 0 },
		func(descriptor *RuntimeMatrixDescriptor) {
			descriptor.Source.End.Line = runtimeMatrixMaxSourceCoordinate + 1
		},
	} {
		invalid := descriptor
		change(&invalid)
		if _, err := EncodeRuntimeMatrixDescriptor(invalid); err == nil {
			t.Fatalf("EncodeRuntimeMatrixDescriptor(%#v) unexpectedly succeeded", invalid)
		}
	}
	if _, err := DecodeRuntimeMatrixDescriptor(bytes.Repeat([]byte{' '}, MaxRuntimeMatrixDescriptorBytes+1)); err == nil || !strings.Contains(err.Error(), "maximum is 16384") {
		t.Fatalf("oversized descriptor error = %v", err)
	}

	schemaSource, err := os.ReadFile(filepath.Join("..", "..", "schemas", "runtime-matrix-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument, descriptorDocument any
	if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(first, &descriptorDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(RuntimeMatrixSchemaV1, schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(RuntimeMatrixSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(descriptorDocument); err != nil {
		t.Fatalf("runtime matrix descriptor does not validate against v1 schema: %v", err)
	}
	namespaced := descriptor
	namespaced.Job = "caller.django"
	namespaced.ProducerJob = "caller.build_django_matrix"
	namespacedSource, err := EncodeRuntimeMatrixDescriptor(namespaced)
	if err != nil {
		t.Fatalf("encode namespaced descriptor: %v", err)
	}
	var namespacedDocument map[string]any
	if err := json.Unmarshal(namespacedSource, &namespacedDocument); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(namespacedDocument); err != nil {
		t.Fatalf("namespaced descriptor does not validate against v1 schema: %v", err)
	}
	namespacedDocument["job"] = `caller\django`
	if err := schema.Validate(namespacedDocument); err == nil {
		t.Fatal("backslash-containing descriptor unexpectedly validates against v1 schema")
	}
}

func validRuntimeMatrixDescriptor(shape string) RuntimeMatrixDescriptor {
	return RuntimeMatrixDescriptor{
		Schema:          RuntimeMatrixSchemaV1,
		Job:             "django",
		Shape:           shape,
		ProducerJob:     "build_django_matrix",
		ProducerStepKey: "gha-build_django_matrix",
		ProducerOutput:  "include",
		SourcePath:      ".github/workflows/ci-backend.yml",
		SourceDigest:    "sha256:" + strings.Repeat("a", 64),
		Source:          workflow.Span{Start: workflow.Position{Line: 2568, Column: 18}, End: workflow.Position{Line: 2568, Column: 78}},
		MaxOutputBytes:  MaxRuntimeMatrixBytes,
		MaxInstances:    MaxRuntimeMatrixInstances,
		MaxGraphJobs:    MaxRuntimeMatrixGraphJobs,
	}
}
