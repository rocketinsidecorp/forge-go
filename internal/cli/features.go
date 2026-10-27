package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// applicationFeatures reads app/bootstrap/app_gen.go, the features the
// generators set (scaffold.AppFile). present is false when the application
// has none: one without a database, or one created before the file existed.
// A file edited by hand is scaffold.ErrAppEdited.
func applicationFeatures(projectRoot *os.Root) (features scaffold.AppFeatures, present bool, err error) {
	if err := rejectExistingSymlinkPaths(projectRoot, []string{scaffold.AppFile}); err != nil {
		return scaffold.AppFeatures{}, false, err
	}
	source, err := projectRoot.ReadFile(scaffold.AppFile)
	if errors.Is(err, fs.ErrNotExist) {
		return scaffold.AppFeatures{}, false, nil
	}
	if err != nil {
		return scaffold.AppFeatures{}, false, err
	}
	if features, err = scaffold.ParseApp(source); err != nil {
		return scaffold.AppFeatures{}, false, err
	}
	// The migration sets (db/migration_sets_gen.go) say whether the job
	// schedules and the job queue are composed.
	if err := rejectExistingSymlinkPaths(projectRoot, []string{scaffold.SetsFile}); err != nil {
		return scaffold.AppFeatures{}, false, err
	}
	sets, err := projectRoot.ReadFile(scaffold.SetsFile)
	if err != nil {
		return scaffold.AppFeatures{}, false, fmt.Errorf("%s: %w", filepath.ToSlash(scaffold.SetsFile), err)
	}
	parsed, err := scaffold.ParseSets(sets)
	if err != nil {
		return scaffold.AppFeatures{}, false, err
	}
	features.Jobs = parsed.Jobs
	features.Queue = parsed.Queue
	return features, true, nil
}

// hasOrganizations reports whether an application with Forge identity is
// multi-tenant (decision D33): app_gen.go says so, and an application from
// before app_gen.go existed, whose identity was always multi-tenant, is too.
func hasOrganizations(projectRoot *os.Root) (bool, error) {
	features, present, err := applicationFeatures(projectRoot)
	if err != nil {
		return false, err
	}
	return !present || features.Organizations, nil
}
