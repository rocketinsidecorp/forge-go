package cli

import (
	"errors"
	"io/fs"
	"os"

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
	features, err = scaffold.ParseApp(source)
	return features, err == nil, err
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
