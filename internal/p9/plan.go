package p9

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"ollie/pkg/tools"
)

// filePlanBackend implements tools.PlanBackend by writing a markdown checklist
// to the planning directory. The file persists across crashes so another
// session can pick up where the previous one left off.
//
// Filename: YYYYMMDDThhmmss_{uid8}--{goal-slugified}__todo.md
type filePlanBackend struct {
	dir string // planning directory path
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
		if i := strings.LastIndex(s, "-"); i > 20 {
			s = s[:i]
		}
	}
	return s
}

// CreatePlan writes a markdown checklist to disk and returns step IDs.
func (b *filePlanBackend) CreatePlan(_ context.Context, goal string, steps []tools.PlanStep) ([]string, string, error) {
	order, err := topoSort(steps)
	if err != nil {
		return nil, "", fmt.Errorf("file plan: %w", err)
	}

	slug := slugify(goal)
	uid := make([]byte, 4)
	rand.Read(uid) //nolint:errcheck
	ts := time.Now().Format("20060102T150405")
	filename := ts + "_" + fmt.Sprintf("%08x", uid) + "--" + slug + "__todo.md"

	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", goal)
	ids := make([]string, len(steps))
	for _, idx := range order {
		step := steps[idx]
		ids[idx] = fmt.Sprintf("s%d", idx+1)
		fmt.Fprintf(&md, "- [ ] %s\n", step.Title)
		if step.Body != "" {
			for _, line := range strings.Split(step.Body, "\n") {
				fmt.Fprintf(&md, "  %s\n", line)
			}
		}
	}

	if err := os.MkdirAll(b.dir, 0755); err != nil {
		return nil, "", fmt.Errorf("file plan: %w", err)
	}
	path := b.dir + "/" + filename
	if err := os.WriteFile(path, []byte(md.String()), 0644); err != nil {
		return nil, "", fmt.Errorf("file plan: %w", err)
	}

	msg := fmt.Sprintf(
		"Plan saved to pl/%s (%d steps). Rename __todo → __wip when you start, mark items [x] as you complete them (e.g. sed -i 's/- \\[ \\] Step title/- [x] Step title/' planfile.md), then rename __wip → __done when the goal is realized.",
		filename, len(steps),
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
