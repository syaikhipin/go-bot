package plan

// View separates the durable mode from a selection that has not reached the
// next Agent step boundary yet.
type View struct {
	Active  bool  `json:"active"`
	Pending *bool `json:"pending,omitempty"`
}

// Policy is guidance only. Sandbox and tool authorization remain independent.
const Policy = "You are in plan mode. Explore the request and design a solution before executing it. Present the complete markdown plan for user review with exit_plan_mode. Do not assume the plan is approved until that tool confirms approval."
