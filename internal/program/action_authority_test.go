package program

import "testing"

func TestInventoryActionAuthorityRefinesOrderedKnownDefaults(t *testing.T) {
	aDefault := Site{Source: "${{ false }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	bDefault := Site{Source: "${{ fromJSON(inputs.a) && github.token || '' }}", Surface: SurfaceActionInputDefault, Result: ResultString, Provenance: ProvenanceAction, Purpose: PurposeExpression}
	action := Action{Runtime: "node24", Inputs: []ActionInput{
		{Name: "a", Default: &aDefault},
		{Name: "b", Default: &bDefault},
	}}
	authority, err := InventoryActionAuthority(map[string]Action{"root": action}, "root", nil, ActionAuthorityOptions{ServerURL: "https://github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false earlier default granted github.token to a later default")
	}
}
