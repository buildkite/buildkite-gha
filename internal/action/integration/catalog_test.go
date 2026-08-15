package integration

import (
	"strings"
	"testing"
)

func TestLookupMatchesKnownCanonicalActions(t *testing.T) {
	tests := []struct {
		identity Identity
		want     Descriptor
	}{
		{Identity{Source: "github", Repository: "actions/checkout"}, Descriptor{Adapter: AdapterCheckoutExactEventSHA}},
		{Identity{Source: "github", Repository: "actions/cache"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "restore"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "save"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/upload-artifact"}, Descriptor{Adapter: AdapterUploadArtifactBuildkite}},
		{Identity{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}, Descriptor{Service: ServiceArtifact}},
		{Identity{Source: "github", Repository: "actions/download-artifact"}, Descriptor{Adapter: AdapterDownloadArtifactBuildkite}},
		{Identity{Source: "github", Repository: "actions/setup-node"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-java"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-python"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-go"}, Descriptor{CacheClientCompatibility: true}},
		{Identity{Source: "github", Repository: "actions/setup-dotnet"}, Descriptor{CacheClientCompatibility: true}},
	}
	for _, test := range tests {
		name := test.identity.Repository
		if test.identity.Path != "" {
			name += "/" + test.identity.Path
		}
		t.Run(name, func(t *testing.T) {
			got, ok := Lookup(test.identity)
			if !ok || got != test.want {
				t.Fatalf("Lookup(%#v) = %#v, %t, want %#v, true", test.identity, got, ok, test.want)
			}
		})
	}
}

func TestAdmitEnforcesCatalogCommitPolicy(t *testing.T) {
	invalidCommit := strings.Repeat("0", 40)
	for _, test := range []struct {
		name     string
		identity Identity
		commit   string
		want     Descriptor
	}{
		{name: "checkout", identity: Identity{Source: "github", Repository: "actions/checkout"}, commit: CheckoutV7Commit, want: Descriptor{Adapter: AdapterCheckoutExactEventSHA}},
		{name: "upload artifact", identity: Identity{Source: "github", Repository: "actions/upload-artifact"}, commit: UploadArtifactV7Commit, want: Descriptor{Adapter: AdapterUploadArtifactBuildkite}},
		{name: "download artifact", identity: Identity{Source: "github", Repository: "actions/download-artifact"}, commit: DownloadArtifactV8Commit, want: Descriptor{Adapter: AdapterDownloadArtifactBuildkite}},
		{name: "cache", identity: Identity{Source: "github", Repository: "actions/cache"}, commit: CacheV4Commit, want: Descriptor{Service: ServiceCache}},
		{name: "cache restore", identity: Identity{Source: "github", Repository: "actions/cache", Path: "restore"}, commit: CacheV4Commit, want: Descriptor{Service: ServiceCache}},
		{name: "cache save", identity: Identity{Source: "github", Repository: "actions/cache", Path: "save"}, commit: CacheV4Commit, want: Descriptor{Service: ServiceCache}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := Admit(test.identity, test.commit)
			if err != nil || !ok || got != test.want {
				t.Fatalf("Admit(%#v, %q) = %#v, %t, %v, want %#v, true, nil", test.identity, test.commit, got, ok, err, test.want)
			}
			got, ok, err = Admit(test.identity, invalidCommit)
			if err == nil || !ok || got != test.want {
				t.Fatalf("Admit(%#v, invalid) = %#v, %t, %v, want %#v, true, error", test.identity, got, ok, err, test.want)
			}
		})
	}
}

func TestAdmitAllowsCatalogEntriesWithoutCommitPolicy(t *testing.T) {
	for _, identity := range []Identity{
		{Source: "github", Repository: "actions/upload-artifact", Path: "merge"},
		{Source: "github", Repository: "actions/setup-node"},
	} {
		if _, ok, err := Admit(identity, ""); err != nil || !ok {
			t.Fatalf("Admit(%#v, empty) found = %t, error = %v, want true, nil", identity, ok, err)
		}
	}
	if descriptor, ok, err := Admit(Identity{Source: "github", Repository: "owner/action"}, strings.Repeat("0", 40)); err != nil || ok || descriptor != (Descriptor{}) {
		t.Fatalf("Admit(unknown) = %#v, %t, %v, want zero, false, nil", descriptor, ok, err)
	}
}

func TestLookupDoesNotBroadenCanonicalIdentity(t *testing.T) {
	for _, identity := range []Identity{
		{Source: "workspace", Repository: "actions/checkout"},
		{Source: "github", Repository: "Actions/Checkout"},
		{Source: "github", Repository: "actions/checkout", Path: "nested"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "actions/upload-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/download-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/setup-node", Path: "nested"},
		{Source: "github", Repository: "actions/setup-ruby"},
		{Source: "workspace", Repository: "actions/setup-node"},
		{Source: "github", Repository: "owner/action"},
	} {
		if descriptor, ok := Lookup(identity); ok {
			t.Fatalf("Lookup(%#v) = %#v, true, want no integration", identity, descriptor)
		}
	}
}
