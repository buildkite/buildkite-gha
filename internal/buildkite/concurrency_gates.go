package buildkite

import "fmt"

type preparedConcurrencyGate struct {
	ConcurrencyGate
	ParentID, OpenKey, CloseKey string
	Members                     []string
	Prerequisites               []string
}

func prepareReusableConcurrencyGates(jobs []Job) ([]preparedConcurrencyGate, error) {
	var gates []preparedConcurrencyGate
	indexes := make(map[string]int)
	jobsByKey := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		jobsByKey[job.Key] = job
		parentID := ""
		seen := make(map[string]bool, len(job.ConcurrencyGates))
		for _, declared := range job.ConcurrencyGates {
			if declared.ID == "" || declared.Group == "" || len(declared.Group) > maxConcurrencyGroupLength {
				return nil, fmt.Errorf("job %q has invalid reusable-workflow concurrency gate", job.Key)
			}
			if seen[declared.ID] {
				return nil, fmt.Errorf("job %q repeats reusable-workflow concurrency gate %q", job.Key, declared.ID)
			}
			seen[declared.ID] = true
			index, exists := indexes[declared.ID]
			if !exists {
				index = len(gates)
				indexes[declared.ID] = index
				gates = append(gates, preparedConcurrencyGate{
					ConcurrencyGate: ConcurrencyGate{ID: declared.ID, Group: declared.Group, Queue: job.Queue},
					ParentID:        parentID,
				})
			} else if gates[index].Group != declared.Group || gates[index].ParentID != parentID {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q has inconsistent membership", declared.ID)
			}
			gates[index].Members = append(gates[index].Members, job.Key)
			parentID = declared.ID
		}
	}
	for _, gate := range gates {
		for parentID := gate.ParentID; parentID != ""; {
			parent := gates[indexes[parentID]]
			if parent.Group == gate.Group {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with enclosing gate %q", gate.ID, parent.ID)
			}
			parentID = parent.ParentID
		}
	}
	for index := range gates {
		gate := &gates[index]
		members := make(map[string]bool, len(gate.Members))
		for _, key := range gate.Members {
			job := jobsByKey[key]
			if job.Concurrency > 0 && job.ConcurrencyGroup == gate.Group {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with member job %q", gate.ID, key)
			}
			members[key] = true
		}
		prerequisites := make(map[string]bool)
		for _, key := range gate.Members {
			for _, dependency := range jobsByKey[key].Dependencies {
				if !members[dependency] && !prerequisites[dependency] {
					prerequisites[dependency] = true
					gate.Prerequisites = append(gate.Prerequisites, dependency)
				}
			}
		}
		// A prerequisite call can include nested gates even when its leaf
		// jobs have no job-level concurrency. Check the full needs closure.
		pending := append([]string(nil), gate.Prerequisites...)
		seen := make(map[string]bool)
		for len(pending) != 0 {
			key := pending[0]
			pending = pending[1:]
			if seen[key] {
				continue
			}
			seen[key] = true
			job := jobsByKey[key]
			if job.Concurrency > 0 && job.ConcurrencyGroup == gate.Group {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with prerequisite job %q", gate.ID, key)
			}
			for _, prerequisiteGate := range job.ConcurrencyGates {
				if prerequisiteGate.Group == gate.Group {
					return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with prerequisite gate %q", gate.ID, prerequisiteGate.ID)
				}
			}
			pending = append(pending, job.Dependencies...)
		}
	}
	return gates, nil
}

func reusableGateOpenDependencies(workflow preparedWorkflow, gate preparedConcurrencyGate, openKeys map[string]string, compilerStep string) []dependency {
	seen := make(map[string]bool)
	var dependencies []dependency
	add := func(step string, allowFailure bool) {
		if step != "" && !seen[step] {
			seen[step] = true
			dependencies = append(dependencies, dependency{Step: step, AllowFailure: allowFailure})
		}
	}
	switch {
	case gate.ParentID != "":
		add(openKeys[gate.ParentID], false)
	case workflow.GateOpenKey != "":
		add(workflow.GateOpenKey, false)
	case !workflow.Aggregate:
		add(compilerStep, false)
	}
	for _, key := range gate.Prerequisites {
		add(key, true)
	}
	return dependencies
}

func reusableGateCloseDependencies(workflow preparedWorkflow, gate preparedConcurrencyGate) []dependency {
	dependencies := make([]dependency, 0, len(gate.Members)+len(workflow.ReusableConcurrencyGates))
	for _, member := range gate.Members {
		dependencies = append(dependencies, dependency{Step: member, AllowFailure: true})
	}
	for _, child := range workflow.ReusableConcurrencyGates {
		if child.ParentID == gate.ID {
			dependencies = append(dependencies, dependency{Step: child.CloseKey, AllowFailure: true})
		}
	}
	return dependencies
}

func workflowGateCloseDependencies(workflow preparedWorkflow, compilerStep string) []dependency {
	dependencies := make([]dependency, 0, len(workflow.Jobs)+len(workflow.ReusableConcurrencyGates)+1)
	if !workflow.Aggregate {
		dependencies = append(dependencies, dependency{Step: compilerStep})
	}
	for _, job := range workflow.Jobs {
		dependencies = append(dependencies, dependency{Step: job.Key, AllowFailure: true})
	}
	for _, gate := range workflow.ReusableConcurrencyGates {
		if gate.ParentID == "" {
			dependencies = append(dependencies, dependency{Step: gate.CloseKey, AllowFailure: true})
		}
	}
	return dependencies
}

// Lifting prerequisites to gate-open can introduce cycles that the authored
// job DAG does not contain. Include both emitted dependencies and queue order:
// every entry in an ordered concurrency group waits for the preceding entry,
// even when that entry is itself blocked on dependencies.
func validateConcurrencyDependencies(workflows []preparedWorkflow, compilerStep string) error {
	dependencies := make(map[string][]string)
	previous := make(map[string]string)
	var order []string
	add := func(key, group string, needs []dependency) {
		order = append(order, key)
		for _, need := range needs {
			dependencies[key] = append(dependencies[key], need.Step)
		}
		if group != "" {
			if prior := previous[group]; prior != "" {
				dependencies[key] = append(dependencies[key], prior)
			}
			previous[group] = key
		}
	}
	for _, workflow := range workflows {
		if workflow.ConcurrencyGate != nil {
			add(workflow.GateOpenKey, workflow.ConcurrencyGate.Group, nil)
			add(workflow.GateCloseKey, workflow.ConcurrencyGate.Group, workflowGateCloseDependencies(workflow, compilerStep))
		}
		openKeys := make(map[string]string)
		for _, gate := range workflow.ReusableConcurrencyGates {
			add(gate.OpenKey, gate.Group, reusableGateOpenDependencies(workflow, gate, openKeys, compilerStep))
			openKeys[gate.ID] = gate.OpenKey
			add(gate.CloseKey, gate.Group, reusableGateCloseDependencies(workflow, gate))
		}
		for _, job := range workflow.Jobs {
			add(job.Key, job.ConcurrencyGroup, jobDependencies(workflow, job, openKeys, compilerStep))
		}
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("concurrency queue and prerequisite dependencies form a cycle at step %q", key)
		}
		if visited[key] {
			return nil
		}
		visiting[key] = true
		for _, need := range dependencies[key] {
			if err := visit(need); err != nil {
				return err
			}
		}
		delete(visiting, key)
		visited[key] = true
		return nil
	}
	for _, key := range order {
		if err := visit(key); err != nil {
			return err
		}
	}
	return nil
}
