package p9

import (
	"context"
	"fmt"
	"strings"

	"ollie/pkg/agent"
	"ollie/pkg/tools"
)

// queuePlanBackend implements tools.PlanBackend by enqueuing each step as a
// prompt into the session's queue via agent.Core.Queue. It is used as the
// reasoning_plan fallback when no task_create MCP tool is available.
//
// Steps are enqueued in topological order (blockers before dependents). The
// goal description is prepended to the first step for context. Placeholder IDs
// ("q1", "q2", …) are returned so the agent can refer to steps by name.
type queuePlanBackend struct {
	core agent.Core
}

// CreatePlan enqueues each plan step as a prompt. Steps with After dependencies
// are sorted so blockers are enqueued before the steps that depend on them.
// The returned IDs are positional placeholders ("q1", "q2", …).
func (b *queuePlanBackend) CreatePlan(_ context.Context, goal string, steps []tools.PlanStep) ([]string, string, error) {
	order, err := topoSort(steps)
	if err != nil {
		return nil, "", fmt.Errorf("queue plan: %w", err)
	}

	ids := make([]string, len(steps))
	for i := range steps {
		ids[i] = fmt.Sprintf("q%d", i+1)
	}

	for pos, idx := range order {
		step := steps[idx]
		var sb strings.Builder
		if pos == 0 {
			sb.WriteString("Goal: ")
			sb.WriteString(goal)
			sb.WriteString("\n\n")
		}
		sb.WriteString("Step ")
		sb.WriteString(ids[idx])
		sb.WriteString(": ")
		sb.WriteString(step.Title)
		if step.Body != "" {
			sb.WriteString("\n")
			sb.WriteString(step.Body)
		}
		if len(step.After) > 0 {
			sb.WriteString("\n(after: ")
			for i, dep := range step.After {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(ids[dep])
			}
			sb.WriteString(")")
		}

		b.core.Queue(sb.String())
	}

	msg := fmt.Sprintf(
		"Plan queued (%d steps). Stop here — do not execute. "+
			"The queue will re-enter you with each step in order.",
		len(steps),
	)
	return ids, msg, nil
}

// topoSort returns the indices of steps in topological order (blockers first).
func topoSort(steps []tools.PlanStep) ([]int, error) {
	n := len(steps)
	visited := make([]bool, n)
	result := make([]int, 0, n)

	var visit func(i int, path []bool) error
	visit = func(i int, path []bool) error {
		if path[i] {
			return fmt.Errorf("dependency cycle at step %d", i)
		}
		if visited[i] {
			return nil
		}
		path[i] = true
		for _, dep := range steps[i].After {
			if dep < 0 || dep >= n {
				continue
			}
			if err := visit(dep, path); err != nil {
				return err
			}
		}
		path[i] = false
		visited[i] = true
		result = append(result, i)
		return nil
	}

	for i := range steps {
		if err := visit(i, make([]bool, n)); err != nil {
			return nil, err
		}
	}
	return result, nil
}
