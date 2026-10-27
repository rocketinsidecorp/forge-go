package cli

import (
	"errors"
	"os"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// agentsRefresh is what a generator that changes the application's
// features does with AGENTS.md: content is the new guide to write, or note
// says why it was left alone. Both are empty when there is nothing to do.
type agentsRefresh struct {
	content []byte
	note    string
}

// planAgentsRefresh renders AGENTS.md for the application before and after a
// generator changes it (before and after adjust a copy of base). The file is
// rewritten only when it is exactly what `forge new` wrote for before, as
// app_gen.go is: a guide edited by hand stays as it is, with a note, and an
// application generated before the guide existed gets none.
func planAgentsRefresh(projectRoot *os.Root, base scaffold.Options, before, after func(*scaffold.Options)) (agentsRefresh, error) {
	current, err := projectRoot.ReadFile(scaffold.AgentsFile)
	if errors.Is(err, os.ErrNotExist) {
		return agentsRefresh{}, nil
	}
	if err != nil {
		return agentsRefresh{}, err
	}
	if _, err := projectRoot.Lstat("tsconfig.json"); err != nil {
		base.JS = true
	}
	was, now := base, base
	before(&was)
	after(&now)
	old, err := scaffold.RenderAgents(was)
	if err != nil {
		return agentsRefresh{}, err
	}
	updated, err := scaffold.RenderAgents(now)
	if err != nil {
		return agentsRefresh{}, err
	}
	switch string(current) {
	case string(updated):
		return agentsRefresh{}, nil
	case string(old):
		return agentsRefresh{content: updated}, nil
	}
	return agentsRefresh{note: scaffold.AgentsFile + " was edited by hand, so it was left as it is: update what it says about sign-in, organizations and generator scopes yourself (`forge new` writes the current text)."}, nil
}
