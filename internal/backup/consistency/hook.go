package consistency

import (
	"sort"
)

// planHook runs the recipe's own dump command with the app's environment;
// see Hook for why that exposes nothing new.
func (p *Planner) planHook(app App, h *Hook, plan *Plan) ([]string, error) {
	env := make([]string, 0, len(app.Env))
	for k, v := range app.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	plan.Dump = &Dump{
		Name:        "hook",
		File:        h.File,
		Consistency: h.Consistency,
		Spec:        command{argv: h.Dump, env: env}.spec(app),
	}
	return h.Covers, nil
}
