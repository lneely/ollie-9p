package p9

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ollie/pkg/tools"
)

// queuePlanBackend implements tools.PlanBackend by enqueuing each step as a
// prompt into the session's queue. It is used as a fallback when no task_create
// MCP tool is available.
//
// Steps are enqueued in dependency order (topological). The goal description is
// prepended to the first step so the agent has context. Placeholder IDs
// ("q1", "q2", …) are returned so the agent can refer to steps by name.
type queuePlanBackend struct {
	enqueuePath string // absolute path to the session's enqueue file
}

// CreatePlan enqueues each plan step as a prompt. Steps with After dependencies
// are sorted so blockers are enqueued before the steps that depend on them.
// The returned IDs are positional placeholders ("q1", "q2", …).
func (b *queuePlanBackend) CreatePlan(_ context.Context, goal string, steps []tools.PlanStep) ([]string, error) {
	// Topological sort so each step is enqueued after all its blockers.
	order, err := topoSort(steps)
	if err != nil {
		return nil, fmt.Errorf("queue plan: %w", err)
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

		if err := os.WriteFile(b.enqueuePath, []byte(sb.String()+"\n"), 0644); err != nil {
			return nil, fmt.Errorf("enqueue step %s: %w", ids[idx], err)
		}
	}
	return ids, nil
}

// topoSort returns the indices of steps in topological order (blockers first).
// It accepts the after-indices directly from PlanStep.After.
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
