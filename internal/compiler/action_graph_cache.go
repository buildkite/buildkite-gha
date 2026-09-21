package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/buildkite/buildkite-gha/internal/program"
)

// actionGraphCache belongs to one bundle and is used sequentially. Only
// completed graphs are retained; invocation inputs and authority are never cached.
type actionGraphCache struct {
	options Options
	graphs  map[actionGraphKey]actionGraph
}

type actionGraphKey struct {
	workspace, workflowPath, workflowDigest, refs string
	remote                                        RemoteWorkflowSource
	isRemote                                      bool
}

func newActionGraphCache(options Options) *actionGraphCache {
	options.ActionSource = newMemoizedActionSource(options.ActionSource)
	return &actionGraphCache{options: options, graphs: make(map[actionGraphKey]actionGraph)}
}

func (cache *actionGraphCache) compile(ctx context.Context, instance JobInstance, serverURL string, refs []string, inputs []map[string]string) (actionCompilation, error) {
	if inputs != nil && len(inputs) != len(refs) {
		return actionCompilation{}, fmt.Errorf("action references and supplied inputs have different lengths")
	}
	// JSON preserves reference order, duplicates, and string boundaries.
	encoded, _ := json.Marshal(refs)
	key := actionGraphKey{workspace: instance.RepositoryRoot, workflowPath: instance.SourcePath, workflowDigest: instance.SourceDigest, refs: string(encoded)}
	if instance.RemoteWorkflow != nil {
		key.remote, key.isRemote = *instance.RemoteWorkflow, true
	}
	graph, ok := cache.graphs[key]
	if !ok {
		var err error
		graph, err = buildActionGraph(ctx, instance.RepositoryRoot, cache.options.ActionSource, refs, workflowSourceResolver(instance, cache.options))
		if err != nil {
			return actionCompilation{}, err
		}
		cache.graphs[key] = graph
	}
	compiled, err := graph.analyzeInvocations(serverURL, refs, inputs)
	if err != nil {
		return actionCompilation{}, err
	}
	// Plans own their mutable data independently of the cached graph.
	compiled.programs = make(map[string]program.Action, len(graph.programs))
	for id, action := range graph.programs {
		compiled.programs[id] = action.Clone()
	}
	compiled.capabilities = slices.Clone(graph.capabilities)
	compiled.locks = slices.Clone(graph.locks)
	for i := range compiled.locks {
		compiled.locks[i].Children = maps.Clone(compiled.locks[i].Children)
	}
	compiled.cacheSubstitutions = slices.Clone(graph.cacheSubstitutions)
	if graph.workflowSource != nil {
		source := *graph.workflowSource
		compiled.workflowSource = &source
	}
	return compiled, nil
}
