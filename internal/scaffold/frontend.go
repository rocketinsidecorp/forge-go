package scaffold

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/frontend"
)

// typescriptDir is the embedded directory of the TypeScript front end:
// the sources, tsconfig.json and the declarations of Turbo and Stimulus. Its
// files are copied unchanged (the sources compiled besides, see
// compiledFiles), so they may contain {{ }} that belong to comments or to
// other tools.
const typescriptDir = "templates/typescript"

// frontendAssets are the files of the application's front end that are
// copied unchanged: JavaScript with --js, else the TypeScript sources,
// tsconfig.json and the type declarations.
func frontendAssets(opts Options) ([]asset, error) {
	if opts.JS {
		return javascriptAssets, nil
	}
	var result []asset
	err := fs.WalkDir(templateFS, typescriptDir, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative := strings.TrimPrefix(source, typescriptDir+"/")
		target := filepath.FromSlash(relative)
		// Sources and declarations live below app/javascript; tsconfig.json
		// sits at the application root next to go.mod.
		if relative != "tsconfig.json" {
			target = filepath.Join("app", "javascript", target)
		}
		result = append(result, asset{source: source, target: target})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", typescriptDir, err)
	}
	return result, nil
}

// compiledFiles are the JavaScript files compiled from the TypeScript sources
// at generation time, app/javascript/dist/**.js, exactly what `forge build`
// writes (frontend.Sync). A binary built with plain `go build` (the generated
// Dockerfile and CI do) embeds them, so they are committed with the sources.
// There are none with --js or --api.
func compiledFiles(opts Options) ([]BaseFile, error) {
	if opts.API || opts.JS {
		return nil, nil
	}
	sources, err := frontendAssets(opts)
	if err != nil {
		return nil, err
	}
	var result []BaseFile
	for _, asset := range sources {
		relative, ok := strings.CutPrefix(filepath.ToSlash(asset.target), frontend.SourceDir+"/")
		if !ok || !strings.HasSuffix(relative, ".ts") || strings.HasSuffix(relative, ".d.ts") {
			continue
		}
		source, err := templateFS.ReadFile(asset.source)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", asset.source, err)
		}
		compiled, err := frontend.Compile(relative, source, frontend.Production)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", asset.source, err)
		}
		result = append(result, BaseFile{
			Path:    filepath.FromSlash(path.Join(frontend.SourceDir, frontend.DistDir, frontend.DistName(relative))),
			Content: compiled,
		})
	}
	return result, nil
}

// TypeScript reports whether the application's front end is TypeScript
// ({{if .TypeScript}}): a full-stack application without --js. A field-only
// condition, as the supply-chain gate allows in the generated workflow.
func (d templateData) TypeScript() bool { return !d.API && !d.JS }

// TypeScriptVersion is the exact tsc release the generated CI installs and
// README names, as templates see it ({{.TypeScriptVersion}}).
func (templateData) TypeScriptVersion() string { return frontend.TypeScriptVersion }
