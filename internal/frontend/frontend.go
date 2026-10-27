// Package frontend compiles a generated application's TypeScript
// (app/javascript/**/*.ts) to the JavaScript the browser runs, with
// esbuild's Go API (ADR 0049). It strips types and nothing else: every file
// is transformed alone, node_modules is never read and import specifiers are
// left exactly as written, because the browser resolves them through the
// import map (ADR 0029). There is no Node toolchain, and no compiler in a
// production binary: the compiled files live in app/javascript/dist, are
// embedded by the application's javascript package and served by the digested
// asset pipeline like any other file.
//
// esbuild does not type-check. `forge check` runs tsc --noEmit when tsc is on
// PATH (typeCheckFrontend in internal/cli).
//
// This package is tooling for the forge command line. The request-handling
// packages never import it (architecture_test.go enforces that).
package frontend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

const (
	// EsbuildVersion is the pinned esbuild release in go.mod. A test fails
	// when the two differ, so the version in the ADR, the docs and the
	// generated files' header cannot drift from what is linked.
	EsbuildVersion = "0.28.2"
	// TypeScriptVersion is the exact tsc release the generated CI installs
	// and `forge check` recommends.
	TypeScriptVersion = "6.0.3"

	// SourceDir is where the application's front end lives, slash-separated.
	SourceDir = "app/javascript"
	// DistDir is the compiled output below SourceDir that the application's
	// javascript package embeds.
	DistDir = "dist"
	// EntryPoint marks a TypeScript front end: the file the layout loads.
	EntryPoint = "application.ts"

	maxSourceBytes = 1 << 20
	maxSources     = 512
)

// Mode selects what Compile produces.
type Mode int

const (
	// Production output is deterministic and carries a header naming its
	// source and the source's SHA-256, which the generated application's
	// own test checks (a stale file fails `go test`).
	Production Mode = iota
	// Development output carries an inline source map instead, so the
	// browser's developer tools show the TypeScript. `forge dev` builds it
	// into a temporary overlay and never writes it to the working tree.
	Development
)

// tsconfigRaw is passed to esbuild in place of reading tsconfig.json, so
// compiling does not depend on the working directory. It repeats the options
// of the generated tsconfig.json that change emitted code: the class field
// semantics of ES2022, and import elision as verbatimModuleSyntax defines it
// (an import without `type` is kept), which is what tsc checks.
const tsconfigRaw = `{"compilerOptions":{"target":"es2022","useDefineForClassFields":true,"verbatimModuleSyntax":true}}`

// headerPattern matches the first line of a Production output.
var headerPattern = regexp.MustCompile(`^// Compiled from (\S+) by forge \([a-z]+ [0-9.]+\); do not edit\. source-sha256: ([0-9a-f]{64}) output-sha256: ([0-9a-f]{64})\r?\n`)

// sourceName is what a source path below app/javascript may look like. It is
// what the header can carry (no whitespace), what the asset table accepts as a
// logical name (letters, digits, '.', '_' and '-') and what go:embed embeds
// (no component starting with '.' or '_').
var sourceName = regexp.MustCompile(`^[A-Za-z0-9-][A-Za-z0-9._-]*(/[A-Za-z0-9-][A-Za-z0-9._-]*)*\.ts$`)

// Header is the first line of a Production output for the source named name
// (slash-separated, relative to app/javascript) with the given content,
// compiled to body. It carries both hashes: the source's, which tells a stale
// file from a fresh one, and the body's, which tells a hand-edited or
// truncated file from the one forge wrote.
func Header(name string, source, body []byte) string {
	return fmt.Sprintf("// Compiled from %s by forge (esbuild %s); do not edit. source-sha256: %s output-sha256: %s\n", name, EsbuildVersion, SourceHash(source), SourceHash(body))
}

// SourceHash is the lowercase hex SHA-256 of a source file or a compiled body,
// with CRLF line ends read as LF: Git on Windows (core.autocrlf) may check the
// same file out with either, and the hash must not flip with it.
func SourceHash(source []byte) string {
	sum := sha256.Sum256(bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// ParseHeader returns the source name and the source hash a Production output
// declares, and whether the rest of the file (after the header line) is the
// body whose hash the header records. ok is false for anything that does not
// start with forge's header.
func ParseHeader(output []byte) (name, sourceHash string, intact, ok bool) {
	match := headerPattern.FindSubmatch(output)
	if match == nil {
		return "", "", false, false
	}
	body := output[len(match[0]):]
	return string(match[1]), string(match[2]), SourceHash(body) == string(match[3]), true
}

// DistName is the compiled file of a source: "controllers/x.ts" becomes
// "controllers/x.js".
func DistName(source string) string {
	return strings.TrimSuffix(source, ".ts") + ".js"
}

// Compile transforms one TypeScript module to an ES2022 module. name is the
// source's slash-separated path below app/javascript; it labels diagnostics
// and the source map. Imports are kept as written.
func Compile(name string, source []byte, mode Mode) ([]byte, error) {
	if len(source) > maxSourceBytes {
		return nil, fmt.Errorf("%s: %d bytes exceeds the %d byte limit of a front-end source", name, len(source), maxSourceBytes)
	}
	options := api.TransformOptions{
		Loader:        api.LoaderTS,
		Format:        api.FormatESModule,
		Target:        api.ES2022,
		Charset:       api.CharsetUTF8,
		LegalComments: api.LegalCommentsInline,
		LogLevel:      api.LogLevelSilent,
		Sourcefile:    path.Join(SourceDir, name),
		TsconfigRaw:   tsconfigRaw,
	}
	if mode == Development {
		options.Sourcemap = api.SourceMapInline
		options.SourcesContent = api.SourcesContentInclude
	}
	result := api.Transform(string(source), options)
	if len(result.Errors) > 0 {
		return nil, diagnosticError(result.Errors)
	}
	if mode == Development {
		return result.Code, nil
	}
	return append([]byte(Header(name, source, result.Code)), result.Code...), nil
}

// diagnosticError formats esbuild's messages with the offending line, the way
// the terminal and the development error page show them.
func diagnosticError(messages []api.Message) error {
	formatted := api.FormatMessages(messages, api.FormatMessagesOptions{Kind: api.ErrorMessage, TerminalWidth: 100})
	return errors.New(strings.TrimSpace(strings.Join(formatted, "")))
}

// Sources lists the TypeScript files of the application at root: every .ts
// below app/javascript except declaration files (.d.ts), the dist output, the
// types directory and node_modules, as slash-separated paths below
// app/javascript, sorted. A root without app/javascript has none.
func Sources(root fs.FS) ([]string, error) {
	var sources []string
	err := fs.WalkDir(root, SourceDir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		base := entry.Name()
		if entry.IsDir() {
			if name != SourceDir && (base == DistDir || base == "types" || base == "node_modules" || strings.HasPrefix(base, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(base, ".ts") || strings.HasSuffix(base, ".d.ts") {
			return nil
		}
		if len(sources) == maxSources {
			return fmt.Errorf("more than %d TypeScript files under %s", maxSources, SourceDir)
		}
		relative := strings.TrimPrefix(name, SourceDir+"/")
		if !sourceName.MatchString(relative) {
			return fmt.Errorf("%s: a TypeScript file name may contain only letters, digits, '.', '_' and '-' (not starting a name with '.' or '_'): the name becomes an asset name and is written into the compiled file's header", name)
		}
		sources = append(sources, relative)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(sources)
	return sources, nil
}

// IsTypeScript reports whether the application at root has a TypeScript front
// end: its entry point app/javascript/application.ts exists. An application
// created with `forge new --js` has application.js instead and is left alone.
func IsTypeScript(root fs.FS) bool {
	info, err := fs.Stat(root, path.Join(SourceDir, EntryPoint))
	return err == nil && info.Mode().IsRegular()
}

// Build compiles every source of the application at root and returns the
// outputs by their path below app/javascript/dist ("application.js",
// "controllers/hello_controller.js"). All errors are reported, not only the
// first file's.
func Build(root fs.FS, mode Mode) (map[string][]byte, error) {
	sources, err := Sources(root)
	if err != nil {
		return nil, err
	}
	outputs := make(map[string][]byte, len(sources))
	var failures []error
	for _, name := range sources {
		source, err := fs.ReadFile(root, path.Join(SourceDir, name))
		if err != nil {
			return nil, err
		}
		compiled, err := Compile(name, source, mode)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		outputs[DistName(name)] = compiled
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return outputs, nil
}
