package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func runI18nCheck(t *testing.T, root string, args ...string) (string, string, int) {
	t.Helper()
	report, err := checkTranslations(root)
	if err != nil {
		return "", err.Error(), 1
	}
	var out bytes.Buffer
	if slices.Contains(args, "--json") {
		if err := json.NewEncoder(&out).Encode(report); err != nil {
			t.Fatal(err)
		}
	} else if err := writeI18nReport(&out, report); err != nil {
		t.Fatal(err)
	}
	code := 0
	if report.missingCount+report.pluralCount > 0 {
		code = 1
	}
	return out.String(), "", code
}

func i18nProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.27\n")
	for name, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), content)
	}
	return root
}

func TestI18nCheckReportsMissingPluralAndUnusedKeys(t *testing.T) {
	t.Parallel()
	root := i18nProject(t, map[string]string{
		"config/locales/en.json":            `{"en": {"greeting": "Hello", "inbox": {"one": "%{count} message", "other": "%{count} messages"}, "old": "Unused", "date": {"formats": {"short": "%b %d"}}}}`,
		"config/locales/pt-BR.json":         `{"pt-BR": {"greeting": "Olá", "inbox": {"one": "%{count} mensagem", "other": "%{count} mensagens"}}}`,
		"config/locales/ru.json":            `{"ru": {"greeting": "Привет", "inbox": {"one": "%{count} сообщение", "other": "%{count} сообщений"}, "old": "x", "date": {"formats": {"short": "%d %b"}}}}`,
		"config/locales/posts.en.json":      `{"en": {"posts": {"title": "Posts"}}}`,
		"app/views/posts/index.gohtml":      `<h1>{{t "posts.title"}}</h1><p>{{t "greeting"}}</p>{{with (t "inbox" "count" 2)}}{{.}}{{end}}<p>{{t "posts.subtitle"}}</p>`,
		"app/adapters/htmlui/posts.go":      "package htmlui\n\nfunc x(r *http.Request) string { return i18n.T(r.Context(), \"posts.flash.created\") + i18n.HTML(ctx, \"greeting\") }\n",
		"app/adapters/htmlui/posts_test.go": "package htmlui\n\nvar _ = i18n.T(ctx, \"test.only\")\n",
		"vendor/x/y.gohtml":                 `{{t "vendored.key"}}`,
	})
	output, _, code := runI18nCheck(t, root)
	if code != 1 {
		t.Fatalf("exit code %d, want 1:\n%s", code, output)
	}
	for _, want := range []string{
		"config/locales: en, pt-BR, ru; 5 keys, 5 referenced",
		"en: 2 missing\n  posts.flash.created\n  posts.subtitle\n",
		"pt-BR: 5 missing\n  date.formats.short\n  old\n  posts.flash.created\n  posts.subtitle\n  posts.title\n",
		"ru: plural forms missing\n  inbox: few, many\n",
		"unused: 1",
		"  old\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("report lacks %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"test.only", "vendored.key", "  date.formats.short\n\nunused"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("report contains %q:\n%s", unwanted, output)
		}
	}

	jsonOutput, _, _ := runI18nCheck(t, root, "--json")
	var report struct {
		SchemaVersion int                            `json:"schema_version"`
		Locales       []string                       `json:"locales"`
		Missing       map[string][]string            `json:"missing"`
		PluralForms   map[string]map[string][]string `json:"plural_forms_missing"`
		Unused        []string                       `json:"unused"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || len(report.Locales) != 3 || len(report.Missing["pt-BR"]) != 5 || !slices.Equal(report.PluralForms["ru"]["inbox"], []string{"few", "many"}) || !slices.Equal(report.Unused, []string{"old"}) {
		t.Fatalf("JSON report = %+v", report)
	}
}

func TestI18nCheckRefusesInvalidProjects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"no locales":     {nil, "config/locales"},
		"empty locales":  {map[string]string{"config/locales/README.md": "x"}, "no translation files"},
		"traversal name": {map[string]string{"config/locales/en_US.json": `{}`}, "invalid locale"},
		"invalid json":   {map[string]string{"config/locales/en.json": `{"en": `}, "invalid JSON"},
		"duplicate keys": {map[string]string{"config/locales/en.json": `{"en": {"a": "1", "a": "2"}}`}, "duplicate key"},
		"non canonical":  {map[string]string{"config/locales/pt-br.json": `{"pt-BR": {}}`}, "pt-BR.json"},
	}
	for name, test := range cases {
		root := i18nProject(t, test.files)
		_, message, code := runI18nCheck(t, root)
		if code != 1 || !strings.Contains(message, test.want) {
			t.Errorf("%s: code %d, error %q, want %q", name, code, message, test.want)
		}
	}
	if _, err := checkTranslations(t.TempDir()); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("outside an application: %v", err)
	}
}

func TestRunI18nUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"i18n"}, {"i18n", "missing"}, {"i18n", "check", "--yaml"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "usage: forge i18n check") {
			t.Errorf("%v: code %d, stderr %q", args, code, stderr.String())
		}
	}
}

// TestGeneratedApplicationTranslationsAreComplete runs the check on what
// forge new and the generators write: every key the pages and handlers use
// exists in every generated locale, and nothing generated is unused.
func TestGeneratedApplicationTranslationsAreComplete(t *testing.T) {
	t.Parallel()
	for _, locale := range scaffold.Locales {
		t.Run(locale, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "shop")
			if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir, Database: "postgresql", Locale: locale}); err != nil {
				t.Fatal(err)
			}
			report, err := checkTranslations(dir)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := writeI18nReport(&out, report); err != nil {
				t.Fatal(err)
			}
			if report.missingCount+report.pluralCount != 0 || len(report.Unused) != 0 {
				t.Fatalf("forge new --locale %s:\n%s", locale, out.String())
			}
			// A scaffold adds its English text: complete in English, and
			// listed as missing in the other locale until it is translated.
			if _, err := runScaffold(dir, "post", "title:string", "body:text?", "published:bool?", "--global"); err != nil {
				t.Fatal(err)
			}
			if report, err = checkTranslations(dir); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			_ = writeI18nReport(&out, report)
			if len(report.Missing["en"]) != 0 || len(report.Unused) != 0 {
				t.Fatalf("after scaffold:\n%s", out.String())
			}
			if locale != "en" && (len(report.Missing[locale]) == 0 || !slices.Contains(report.Missing[locale], "posts.index.title")) {
				t.Fatalf("the scaffold's keys are not reported missing in %s:\n%s", locale, out.String())
			}
		})
	}
}
