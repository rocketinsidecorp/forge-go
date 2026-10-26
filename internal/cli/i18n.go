package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

// localesDir is where applications keep their translation files.
var localesDir = filepath.Join("config", "locales")

// Bounds of the reference scan, so a stray large tree cannot stall the check.
const (
	maxScannedFiles     = 20_000
	maxScannedFileBytes = 2 << 20
)

var (
	// templateReference matches the t helper with a literal key:
	// {{t "users.welcome"}}, (t "users.welcome" "name" .Name).
	templateReference = regexp.MustCompile(`(?:^|[\s({|])t\s+"([A-Za-z0-9_.\-]+)"`)
	// goReference matches i18n.T, i18n.HTML, Translate and HTML calls with a
	// literal key as their second argument.
	goReference = regexp.MustCompile(`(?:\bi18n\.(?:T|HTML)|\.(?:Translate|HTML))\(\s*[^,()]*(?:\([^()]*\))?\s*,\s*"([A-Za-z0-9_.\-]+)"`)
	// dataScopes hold formatting data read by l and number, not text.
	dataScopes = []string{"date.", "time.", "number."}
)

var errTranslationsMissing = errors.New("translations are missing")

type i18nReport struct {
	SchemaVersion int                            `json:"schema_version"`
	Locales       []string                       `json:"locales"`
	Keys          int                            `json:"keys"`
	Referenced    int                            `json:"referenced"`
	Missing       map[string][]string            `json:"missing"`
	PluralForms   map[string]map[string][]string `json:"plural_forms_missing"`
	Unused        []string                       `json:"unused"`
	missingCount  int
	pluralCount   int
}

func runI18n(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "check" {
		return usageError{"usage: forge i18n check [--json]"}
	}
	jsonOutput := false
	for _, arg := range args[1:] {
		if arg != "--json" {
			return usageError{"usage: forge i18n check [--json]"}
		}
		jsonOutput = true
	}
	report, err := checkTranslations(".")
	if err != nil {
		return err
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
	} else if err := writeI18nReport(stdout, report); err != nil {
		return err
	}
	if report.missingCount+report.pluralCount > 0 {
		return errTranslationsMissing
	}
	return nil
}

// checkTranslations compares every locale of config/locales with the union
// of all locales' keys and the keys the application references with a
// literal (t "KEY" in templates, i18n.T(ctx, "KEY") in Go).
func checkTranslations(root string) (*i18nReport, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	if _, err := projectRoot.Stat("go.mod"); err != nil {
		return nil, errors.New("go.mod not found: run this inside a Forge application")
	}
	files, err := fs.Sub(projectRoot.FS(), filepath.ToSlash(localesDir))
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (forge new generates it)", localesDir, err)
	}
	var available []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		locale, err := i18n.FileLocale(entry.Name())
		if err != nil {
			return nil, err
		}
		available = append(available, locale)
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("%s has no translation files (LOCALE.json)", localesDir)
	}
	slices.Sort(available)
	defaultLocale := available[0]
	if slices.Contains(available, i18n.DefaultLocale) {
		defaultLocale = i18n.DefaultLocale
	}
	translator, err := i18n.Load(files, i18n.Options{DefaultLocale: defaultLocale, ShowMissing: true})
	if err != nil {
		return nil, err
	}
	referenced, err := scanReferences(projectRoot)
	if err != nil {
		return nil, err
	}

	report := &i18nReport{
		SchemaVersion: 1,
		Locales:       translator.Locales(),
		Missing:       map[string][]string{},
		PluralForms:   map[string]map[string][]string{},
		Unused:        []string{},
		Referenced:    len(referenced),
	}
	all := map[string]struct{}{}
	localeKeys := map[string]map[string]struct{}{}
	for _, locale := range report.Locales {
		localeKeys[locale] = map[string]struct{}{}
		for _, key := range translator.Keys(locale) {
			all[key] = struct{}{}
			localeKeys[locale][key] = struct{}{}
		}
	}
	defined := len(all)
	for key := range referenced {
		all[key] = struct{}{}
	}
	keys := make([]string, 0, len(all))
	for key := range all {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	report.Keys = defined
	for _, locale := range report.Locales {
		required := translator.PluralCategories(locale)
		for _, key := range keys {
			if _, ok := localeKeys[locale][key]; !ok || (!translator.Has(locale, key) && !isDataKey(key)) {
				report.Missing[locale] = append(report.Missing[locale], key)
				report.missingCount++
				continue
			}
			present, plural := translator.PluralForms(locale, key)
			if !plural {
				continue
			}
			var absent []string
			for _, category := range required {
				if !slices.Contains(present, category) {
					absent = append(absent, string(category))
				}
			}
			if len(absent) > 0 {
				if report.PluralForms[locale] == nil {
					report.PluralForms[locale] = map[string][]string{}
				}
				report.PluralForms[locale][key] = absent
				report.pluralCount++
			}
		}
	}
	for _, key := range keys {
		if _, used := referenced[key]; used || isDataKey(key) {
			continue
		}
		report.Unused = append(report.Unused, key)
	}
	return report, nil
}

func isDataKey(key string) bool {
	for _, scope := range dataScopes {
		if strings.HasPrefix(key, scope) {
			return true
		}
	}
	return false
}

// scanReferences collects the literal keys of t, i18n.T and i18n.HTML in the
// application's templates and non-test Go files.
func scanReferences(projectRoot *os.Root) (map[string]struct{}, error) {
	referenced := map[string]struct{}{}
	scanned := 0
	err := fs.WalkDir(projectRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || slices.Contains([]string{"vendor", "node_modules", "tmp", "bin", "storage"}, name)) {
				return fs.SkipDir
			}
			return nil
		}
		var pattern *regexp.Regexp
		switch {
		case strings.HasSuffix(name, ".gohtml"):
			pattern = templateReference
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"):
			pattern = goReference
		default:
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if scanned++; scanned > maxScannedFiles {
			return fmt.Errorf("more than %d source files to scan", maxScannedFiles)
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxScannedFileBytes {
			return err
		}
		content, err := fs.ReadFile(projectRoot.FS(), path)
		if err != nil {
			return err
		}
		for _, match := range pattern.FindAllSubmatch(content, -1) {
			referenced[string(match[1])] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for translation keys: %w", err)
	}
	return referenced, nil
}

func writeI18nReport(output io.Writer, report *i18nReport) error {
	var out strings.Builder
	fmt.Fprintf(&out, "%s: %s; %d keys, %d referenced in templates and Go code\n", filepath.ToSlash(localesDir), strings.Join(report.Locales, ", "), report.Keys, report.Referenced)
	for _, locale := range report.Locales {
		missing := report.Missing[locale]
		forms := report.PluralForms[locale]
		if len(missing) == 0 && len(forms) == 0 {
			fmt.Fprintf(&out, "\n%s: complete\n", locale)
			continue
		}
		if len(missing) > 0 {
			fmt.Fprintf(&out, "\n%s: %d missing\n", locale, len(missing))
			for _, key := range missing {
				fmt.Fprintf(&out, "  %s\n", key)
			}
		}
		if len(forms) > 0 {
			fmt.Fprintf(&out, "\n%s: plural forms missing\n", locale)
			keys := make([]string, 0, len(forms))
			for key := range forms {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				fmt.Fprintf(&out, "  %s: %s\n", key, strings.Join(forms[key], ", "))
			}
		}
	}
	if len(report.Unused) > 0 {
		fmt.Fprintf(&out, "\nunused: %d (no literal t \"KEY\" or i18n.T(ctx, \"KEY\") found; keys built at runtime are not detected)\n", len(report.Unused))
		for _, key := range report.Unused {
			fmt.Fprintf(&out, "  %s\n", key)
		}
	}
	_, err := io.WriteString(output, out.String())
	return err
}
