package sessionformat

import (
	"io"
)

// Inspection joins physical framing, released-v4 row admission, event
// relationships, and model-surface replay. It is deliberately not a live Agent
// Session: the production restore path must still adopt its typed state and
// complete the remaining native payload checks before allowing execution.
type Inspection struct {
	PhysicalArtifact
	Surface    Surface
	Inbox      InboxState
	Goal       GoalProjection
	Plan       PlanProjection
	Children   []SubagentCatalogEntry
	Descriptor *SubagentDescriptor
}

func InspectV4JSONL(input io.Reader) (Inspection, error) {
	artifact, err := DecodePhysicalJSONL(input)
	if err != nil {
		return Inspection{}, err
	}
	return InspectV4Artifact(artifact)
}

// InspectV4Artifact is the read-only history boundary for an already fetched
// canonical v4 log. It does not append resume markers or acquire a writer.
func InspectV4Artifact(artifact PhysicalArtifact) (Inspection, error) {
	if err := ValidateV4Rows(artifact.Events); err != nil {
		return Inspection{}, err
	}
	if err := ValidateV4Relationships(artifact.Events); err != nil {
		return Inspection{}, err
	}
	surface, err := FoldSurface(artifact.Events)
	if err != nil {
		return Inspection{}, err
	}
	inbox, err := FoldInbox(artifact.Events)
	if err != nil {
		return Inspection{}, err
	}
	goal, err := FoldGoal(artifact.Events)
	if err != nil {
		return Inspection{}, err
	}
	plan, err := FoldPlan(artifact.Events)
	if err != nil {
		return Inspection{}, err
	}
	children, err := FoldSubagentCatalog(artifact.Events, artifact.InheritedEventCount)
	if err != nil {
		return Inspection{}, err
	}
	descriptor, err := FoldSubagentDescriptor(artifact.Events)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{PhysicalArtifact: artifact, Surface: surface, Inbox: inbox, Goal: goal, Plan: plan, Children: children, Descriptor: descriptor}, nil
}
