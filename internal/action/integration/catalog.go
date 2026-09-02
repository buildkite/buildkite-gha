package integration

// Identity is the canonical, resolved portion of an action lock used for
// integration matching. The immutable commit and source digest remain runtime
// verification evidence rather than catalog selectors.
type Identity struct {
	Source     string
	Repository string
	Path       string
}

// Adapter identifies a runtime replacement for an action's upstream lifecycle.
type Adapter string

const (
	// AdapterCheckoutExactEventSHA checks out the event repository at its exact
	// event SHA without executing actions/checkout's JavaScript lifecycle.
	AdapterCheckoutExactEventSHA Adapter = "checkout-exact-event-sha-v1"
	// AdapterUploadArtifactBuildkite stores the pinned upload-artifact action as
	// a Buildkite-native ZIP without executing its JavaScript lifecycle.
	AdapterUploadArtifactBuildkite Adapter = "upload-artifact-buildkite-v1"
	// AdapterDownloadArtifactBuildkite extracts one producer-attributed native
	// archive without using the GitHub Actions artifact service.
	AdapterDownloadArtifactBuildkite Adapter = "download-artifact-buildkite-v1"
)

// Service identifies an Actions runtime service used by a known action.
// Service classification supports admission diagnostics only: services may be
// used transitively and must not be provisioned based solely on this catalog.
type Service string

const (
	ServiceArtifact Service = "artifact"
	ServiceCache    Service = "cache"
)

// Descriptor records Buildkite-specific handling for one exact action identity.
type Descriptor struct {
	Adapter Adapter
	Service Service
	// CacheClientCompatibility adapts bundled cache clients to Buildkite's
	// cache-v2 environment. Before enabling it, confirm GITHUB_SERVER_URL
	// affects only caching and ACTIONS_CACHE_URL is only a presence check.
	CacheClientCompatibility bool
}

type catalogEntry struct {
	descriptor     Descriptor
	validateCommit func(string) error
}

var catalog = map[Identity]catalogEntry{
	{Source: "github", Repository: "actions/checkout"}: {
		descriptor:     Descriptor{Adapter: AdapterCheckoutExactEventSHA},
		validateCommit: validateCheckoutCommit,
	},
	{Source: "github", Repository: "actions/cache"}: {
		descriptor:     Descriptor{Service: ServiceCache},
		validateCommit: validateCacheCommit,
	},
	{Source: "github", Repository: "actions/cache", Path: "restore"}: {
		descriptor:     Descriptor{Service: ServiceCache},
		validateCommit: validateCacheCommit,
	},
	{Source: "github", Repository: "actions/cache", Path: "save"}: {
		descriptor:     Descriptor{Service: ServiceCache},
		validateCommit: validateCacheCommit,
	},
	{Source: "github", Repository: "actions/upload-artifact"}: {
		descriptor:     Descriptor{Adapter: AdapterUploadArtifactBuildkite},
		validateCommit: validateUploadArtifactCommit,
	},
	{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}: {
		descriptor: Descriptor{Service: ServiceArtifact},
	},
	{Source: "github", Repository: "actions/download-artifact"}: {
		descriptor:     Descriptor{Adapter: AdapterDownloadArtifactBuildkite},
		validateCommit: validateDownloadArtifactCommit,
	},
	{Source: "github", Repository: "actions/setup-node"}:   {descriptor: Descriptor{CacheClientCompatibility: true}},
	{Source: "github", Repository: "actions/setup-java"}:   {descriptor: Descriptor{CacheClientCompatibility: true}},
	{Source: "github", Repository: "actions/setup-python"}: {descriptor: Descriptor{CacheClientCompatibility: true}},
	{Source: "github", Repository: "actions/setup-go"}:     {descriptor: Descriptor{CacheClientCompatibility: true}},
	{Source: "github", Repository: "actions/setup-dotnet"}: {descriptor: Descriptor{CacheClientCompatibility: true}},
}

// Lookup returns the integration for an exact canonical action identity.
// It does not admit the resolved commit; callers handling action locks should
// use Admit instead.
func Lookup(identity Identity) (Descriptor, bool) {
	entry, ok := catalog[identity]
	return entry.descriptor, ok
}

// Admit returns the integration for an exact canonical action identity and
// atomically enforces any audited-commit policy attached to it.
func Admit(identity Identity, commit string) (Descriptor, bool, error) {
	entry, ok := catalog[identity]
	if !ok {
		return Descriptor{}, false, nil
	}
	if entry.validateCommit != nil {
		if err := entry.validateCommit(commit); err != nil {
			return entry.descriptor, true, err
		}
	}
	return entry.descriptor, true, nil
}

// AdmitNativeAdapter reports whether an exact action release replaces the
// upstream lifecycle with Buildkite-native execution. It applies the same
// audited-commit policy as Admit.
func AdmitNativeAdapter(identity Identity, commit string) (Adapter, bool, error) {
	descriptor, known := Lookup(identity)
	if !known || descriptor.Adapter == "" {
		return "", false, nil
	}
	descriptor, _, err := Admit(identity, commit)
	if err != nil {
		return descriptor.Adapter, false, err
	}
	return descriptor.Adapter, true, nil
}
