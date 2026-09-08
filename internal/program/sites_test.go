package program

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPositionalWalkerVisitsAndTransformsEverySiteFieldExactlyOnce(t *testing.T) {
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
		seen++
		if !strings.HasSuffix(site.Source, "!") {
			t.Fatalf("site %q was not transformed", site.Source)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != marker {
		t.Fatalf("transformed walk visited %d positions, want %d", seen, marker)
	}
	if err := program.walkSites(func(site *Site) error {
		if strings.HasSuffix(site.Source, "!") {
			t.Fatalf("TransformSites mutated the original site %q", site.Source)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
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
		Steps:     []Step{{ID: "step", Kind: "run", Condition: Site{Source: "true"}, Run: &Run{Command: Site{Source: "echo ok"}}}},
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
