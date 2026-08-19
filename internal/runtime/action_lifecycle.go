package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type registeredPost struct {
	condition  string
	invocation *preparedInvocation
}

type postRegistry struct {
	mu    sync.Mutex
	posts []registeredPost
}

type preparedInvocation struct {
	action         javaScriptAction
	state          map[string]string
	node           string
	eval           expression.Context
	envOverlay     map[string]string
	isolated       bool
	postRegistered bool
	preFailure     error
}

type remotePreparations map[string]*preparedInvocation

func bindCompositeInvocationSteps(invocation *preparedInvocation, steps map[string]expression.StepStatus) {
	if invocation != nil && invocation.isolated {
		invocation.eval.Steps = steps
	}
}

type remotePreparationStatus struct {
	unsuccessful bool
}

type remotePreparationTimeout struct {
	step     plan.Step
	eval     expression.Context
	resolved bool
	bounded  context.Context
	cancel   context.CancelFunc
}

func (t *remotePreparationTimeout) context(parent context.Context) (context.Context, error) {
	if !t.resolved {
		step, err := evaluateStepTimeout(t.step, t.eval)
		if err != nil {
			return nil, fmt.Errorf("controls: %w", err)
		}
		t.bounded, t.cancel = stepContext(parent, step.TimeoutMinutes)
		t.resolved = true
	}
	return t.bounded, nil
}

func (t *remotePreparationTimeout) close() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

const node16DeprecationMessage = "Node.js 16 actions are deprecated. Please update the following actions to use Node.js 20: %s. For more information see: https://github.blog/changelog/2023-09-22-github-actions-transitioning-from-node-16-to-node-20/."

type node16DeprecationWarnings struct {
	mu      sync.Mutex
	actions map[string]struct{}
}

func (w *node16DeprecationWarnings) record(reference string) {
	if w == nil || reference == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.actions == nil {
		w.actions = make(map[string]struct{})
	}
	w.actions[reference] = struct{}{}
}

func (w *node16DeprecationWarnings) emit(processor *commandProcessor) {
	if w == nil || processor == nil {
		return
	}
	w.mu.Lock()
	actions := sortedKeys(w.actions)
	w.mu.Unlock()
	if len(actions) != 0 {
		processor.trustedWarning(fmt.Sprintf(node16DeprecationMessage, strings.Join(actions, ", ")))
	}
}

func actionNodeMajor(runtime metadata.Runtime) (int, bool) {
	switch runtime {
	case metadata.RuntimeNode16:
		return 16, true
	case metadata.RuntimeNode24:
		return 24, true
	default:
		return 0, false
	}
}

func (r *postRegistry) register(post *registeredPost) {
	if post == nil {
		return
	}
	r.mu.Lock()
	r.posts = append(r.posts, *post)
	r.mu.Unlock()
}

func (r *postRegistry) snapshot() []registeredPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registeredPost(nil), r.posts...)
}
