package runtime

import (
	"reflect"
	"testing"
)

func TestInvocationEnvironmentProcessOverlayOrder(t *testing.T) {
	var r jobRun
	environment := r.invocationEnvironment(
		map[string]string{"SHARED": "job", "JOB_ONLY": "job", "GITHUB_REPOSITORY": "owner/repo"},
		map[string]string{"SHARED": "step", "STEP_ONLY": "step", "GITHUB_REPOSITORY": "spoofed"},
	)
	got := environment.process()
	want := map[string]string{
		"SHARED":    "step",
		"JOB_ONLY":  "job",
		"STEP_ONLY": "step",
		// Runtime context names stay job-owned even when a step overlays them.
		"GITHUB_REPOSITORY": "owner/repo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("process() = %#v, want %#v", got, want)
	}
}

func TestInvocationEnvironmentDockerPATHPrecedence(t *testing.T) {
	tests := []struct {
		name             string
		runner           jobRun
		jobEnv           map[string]string
		stepEnv          map[string]string
		actionEnv        map[string]string
		wantPATH         string
		wantExplicitPATH bool
	}{
		{
			name:             "action PATH replaces implicit job PATH",
			runner:           jobRun{implicitJobPATH: "/usr/bin"},
			jobEnv:           map[string]string{"PATH": "/usr/bin"},
			actionEnv:        map[string]string{"PATH": "/action/bin"},
			wantPATH:         "/action/bin",
			wantExplicitPATH: true,
		},
		{
			name:             "explicit job PATH wins over action PATH",
			runner:           jobRun{explicitJobPATH: true, implicitJobPATH: "/usr/bin"},
			jobEnv:           map[string]string{"PATH": "/job/bin"},
			actionEnv:        map[string]string{"PATH": "/action/bin"},
			wantPATH:         "/job/bin",
			wantExplicitPATH: true,
		},
		{
			name:             "step PATH wins over action PATH",
			runner:           jobRun{implicitJobPATH: "/usr/bin"},
			jobEnv:           map[string]string{"PATH": "/usr/bin"},
			stepEnv:          map[string]string{"PATH": "/step/bin"},
			actionEnv:        map[string]string{"PATH": "/action/bin"},
			wantPATH:         "/step/bin",
			wantExplicitPATH: true,
		},
		{
			name:             "implicit PATH everywhere keeps image PATH authority",
			runner:           jobRun{implicitJobPATH: "/usr/bin"},
			jobEnv:           map[string]string{"PATH": "/usr/bin"},
			actionEnv:        map[string]string{"OTHER": "value"},
			wantPATH:         "/usr/bin",
			wantExplicitPATH: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := test.runner.invocationEnvironment(test.jobEnv, test.stepEnv)
			env, explicitPATH := environment.docker(test.actionEnv, nil)
			if env["PATH"] != test.wantPATH || explicitPATH != test.wantExplicitPATH {
				t.Fatalf("docker() PATH = %q explicit = %v, want %q explicit = %v", env["PATH"], explicitPATH, test.wantPATH, test.wantExplicitPATH)
			}
		})
	}
}

func TestInvocationEnvironmentDockerOverlaysActionAndInputs(t *testing.T) {
	runner := jobRun{implicitJobPATH: "/usr/bin"}
	environment := runner.invocationEnvironment(
		map[string]string{"PATH": "/usr/bin", "SHARED": "job"},
		map[string]string{"STEP_ONLY": "step"},
	)
	env, _ := environment.docker(
		map[string]string{"SHARED": "action", "ACTION_ONLY": "action"},
		map[string]string{"my input": "value"},
	)
	want := map[string]string{
		"PATH":           "/usr/bin",
		"SHARED":         "job", // action env fills only unset names
		"STEP_ONLY":      "step",
		"ACTION_ONLY":    "action",
		"INPUT_MY_INPUT": "value",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("docker() env = %#v, want %#v", env, want)
	}
}
