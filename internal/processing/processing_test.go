package processing

import "testing"

func TestStageDefinitionsAreValidUniqueAndCopied(t *testing.T) {
	definitions := StageDefinitions()
	if len(definitions) == 0 {
		t.Fatal("StageDefinitions() is empty")
	}

	seen := make(map[Stage]bool, len(definitions))
	for _, definition := range definitions {
		if definition.ID == "" || definition.Name == "" {
			t.Fatalf("incomplete stage definition: %#v", definition)
		}
		if seen[definition.ID] {
			t.Fatalf("duplicate stage definition: %q", definition.ID)
		}
		seen[definition.ID] = true
	}

	definitions[0].Name = "changed"
	if StageDefinitions()[0].Name == "changed" {
		t.Fatal("StageDefinitions returned shared storage")
	}
}
