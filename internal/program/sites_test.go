package program

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPositionalWalkerVisitsAndTransformsEverySiteExactlyOnce(t *testing.T) {
	marker := 0
	site := func() Site {
		marker++
		return Site{Source: strings.Repeat("x", marker)}
	}
	binding := func() Binding { return Binding{Name: "value", Value: site()} }
	boolExpression, numberExpression, dynamicServices := site(), site(), site()
	program := Program{Version: Version, Job: Job{
		Guards: []Guard{{Condition: site()}}, Condition: site(), Env: []Binding{binding()},
		Defaults:  Defaults{Shell: site(), WorkingDirectory: site()},
		Container: &Container{Image: site(), Env: []Binding{binding()}, Ports: []Site{site()}},
		Services: Services{Static: []Service{{Name: "db", Container: ServiceContainer{
			Image: site(), Credentials: &ContainerCredentials{Username: site(), Password: site()},
			Env: []Binding{binding()}, Ports: []Site{site()}, Volumes: []Site{site()},
			Options: site(), Command: site(), Entrypoint: site(),
		}}}, Dynamic: &dynamicServices},
		Steps: []Step{
			{ID: "run", Env: []Binding{binding()}, Condition: site(), ContinueOnError: BoolControl{Expression: &boolExpression}, TimeoutMinutes: NumberControl{Expression: &numberExpression}, Name: site(), Run: &Run{Command: site(), Shell: site(), WorkingDirectory: site()}},
			{ID: "uses", Condition: site(), Name: site(), Invocation: &Invocation{Uses: site(), With: []Binding{binding()}}},
		},
		Outputs: []Binding{binding()},
	}, Actions: map[string]Action{
		"node":   {Runtime: "node24", Inputs: []ActionInput{{Name: "value", Default: sitePointer(site())}}, Outputs: []ActionOutput{{Name: "value", Value: site()}}, PreIf: site(), PostIf: site(), Env: []Binding{binding()}},
		"docker": {Runtime: "docker", Args: []Site{site()}},
		"composite": {Runtime: "composite", Steps: []ActionStep{
			{Name: site(), Condition: site(), Env: []Binding{binding()}, Run: &ActionRun{Command: site()}, Shell: site(), WorkingDirectory: site()},
			{Name: site(), Condition: site(), Invocation: &Invocation{Uses: site(), With: []Binding{binding()}}, Shell: site(), WorkingDirectory: site()},
		}},
	}}

	walked := map[string]int{}
	if err := program.walkSites(func(site *Site) error {
		if site.Source != "" {
			walked[site.Source]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != marker {
		t.Fatalf("walked %d populated positions, want %d", len(walked), marker)
	}
	for source, count := range walked {
		if count != 1 {
			t.Fatalf("site %q walked %d times", source, count)
		}
	}

	transformed, err := program.TransformSites(func(site Site) (Site, error) {
		walked[site.Source]--
		site.Source += "!"
		return site, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for source, count := range walked {
		if count != 0 {
			t.Fatalf("site %q transformed %d times, want once", source, 1-count)
		}
	}
	seen := 0
	if err := transformed.walkSites(func(site *Site) error {
		if site.Source != "" {
			seen++
			if !strings.HasSuffix(site.Source, "!") {
				t.Fatalf("site %q was not transformed", site.Source)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != marker {
		t.Fatalf("transformed walk visited %d positions, want %d", seen, marker)
	}
}

func TestPositionalWalkerCoversEverySiteField(t *testing.T) {
	var program Program
	marker := 0
	populateSiteFields(reflect.ValueOf(&program).Elem(), &marker)

	walked := make(map[string]int, marker)
	if err := program.walkSites(func(site *Site) error {
		walked[site.Source]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != marker {
		t.Fatalf("walker visited %d of %d reflected Site fields", len(walked), marker)
	}
	for source, count := range walked {
		if count != 1 {
			t.Fatalf("site %q walked %d times", source, count)
		}
	}
}

var siteType = reflect.TypeFor[Site]()

func populateSiteFields(value reflect.Value, marker *int) {
	if value.Type() == siteType {
		(*marker)++
		value.Set(reflect.ValueOf(Site{Source: "site-" + strconv.Itoa(*marker)}))
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		populateSiteFields(value.Elem(), marker)
	case reflect.Struct:
		for i := range value.NumField() {
			if value.Field(i).CanSet() {
				populateSiteFields(value.Field(i), marker)
			}
		}
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		populateSiteFields(value.Index(0), marker)
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
		entry := reflect.New(value.Type().Elem()).Elem()
		populateSiteFields(entry, marker)
		value.SetMapIndex(reflect.ValueOf("entry").Convert(value.Type().Key()), entry)
	}
}

func TestProgramWireDerivesSiteSemanticsFromPosition(t *testing.T) {
	program := Program{Version: Version, Job: Job{
		Condition: Site{Source: "true"}, Defaults: Defaults{}, Services: Services{},
		Steps: []Step{{ID: "step", Kind: "run", Condition: Site{Source: "true"}, Run: &Run{Command: Site{Source: "echo ok"}}}},
	}, Actions: map[string]Action{"action": {Runtime: "node24", Main: "index.js", PreIf: Site{Source: "always()"}}}}
	encoded, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	for _, redundant := range []string{`"profile"`, `"result"`, `"provenance"`, `"purpose"`} {
		if strings.Contains(string(encoded), redundant) {
			t.Fatalf("wire program contains positional claim %s", redundant)
		}
	}
	var decoded Program
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded.Job.Condition; got.Surface != SurfaceJobCondition || got.Result != ResultBoolean || got.Provenance != ProvenanceWorkflow || got.Purpose != PurposeExpression {
		t.Fatalf("job condition semantics = %#v", got)
	}
	if got := decoded.Actions["action"].PreIf; got.Surface != SurfaceActionLifecycle || got.Result != ResultBoolean || got.Provenance != ProvenanceAction || got.Purpose != PurposeExpression {
		t.Fatalf("action pre-if semantics = %#v", got)
	}
}

func TestValidateDerivesSiteSemanticsInPlace(t *testing.T) {
	program := Program{Version: Version, Job: Job{
		Condition: Site{Source: "true"},
		Steps:     []Step{{ID: "step", Condition: Site{Source: "true"}, Run: &Run{Command: Site{Source: "echo ok"}}}},
	}}
	if err := program.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := program.Job.Steps[0].Run.Command; got.Surface != SurfaceStepTemplate || got.Result != ResultString || got.Provenance != ProvenanceWorkflow {
		t.Fatalf("validated command semantics = %#v", got)
	}
	if _, err := InventoryAuthority(program, AuthorityOptions{}); err != nil {
		t.Fatalf("InventoryAuthority() after Validate() = %v", err)
	}
}

func sitePointer(site Site) *Site { return &site }
