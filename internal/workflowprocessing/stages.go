package workflowprocessing

// Stage identifies one stable workflow-processing boundary.
type Stage string

const (
	StageWorkflowParsing Stage = "workflow-parsing"
	StageEventValidation Stage = "event-validation"
	StageGraph           Stage = "static-graph-construction"
	StageMatrix          Stage = "matrix-expansion"
	StageExpressions     Stage = "expression-validation"
	StageDiscovery       Stage = "action-discovery"
	StageResolution      Stage = "action-resolution"
	StagePlans           Stage = "job-plan-construction"
	StageAdmission       Stage = "hosted-profile-admission"
	StagePipeline        Stage = "pipeline-generation"
)

// StageDefinition supplies the stable identity and presentation of a stage.
type StageDefinition struct {
	ID   Stage
	Name string
}

var stageDefinitions = [...]StageDefinition{
	{ID: StageWorkflowParsing, Name: "Workflow parsing"},
	{ID: StageEventValidation, Name: "Event validation"},
	{ID: StageGraph, Name: "Static graph construction"},
	{ID: StageMatrix, Name: "Matrix expansion"},
	{ID: StageExpressions, Name: "Expression validation"},
	{ID: StageDiscovery, Name: "Local and public action discovery"},
	{ID: StageResolution, Name: "Immutable action resolution"},
	{ID: StagePlans, Name: "Job-plan construction"},
	{ID: StageAdmission, Name: "Hosted-profile admission"},
	{ID: StagePipeline, Name: "Pipeline generation"},
}

// StageDefinitions returns every stage in processing order.
func StageDefinitions() []StageDefinition {
	return append([]StageDefinition(nil), stageDefinitions[:]...)
}
