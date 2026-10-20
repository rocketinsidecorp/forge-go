package i18n

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// Limits of a translation catalog. Translation files are trusted, but a
// mistake (a generated file, a copy of a huge dataset) should stop the
// application at startup instead of exhausting its memory.
const (
	// MaxFileBytes is the largest translation file Load reads.
	MaxFileBytes = 1 << 20
	// MaxFiles is the most translation files one catalog may have.
	MaxFiles = 512
	// MaxDepth is the deepest nesting of objects below the locale.
	MaxDepth = 16
	// MaxKeysPerLocale bounds the leaf keys of one locale.
	MaxKeysPerLocale = 50_000
	// MaxKeyLength bounds a dotted key.
	MaxKeyLength = 256
)

var (
	segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	scopePattern   = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
)

// tree is one locale's translations: values are string, json.Number, bool,
// []any (of string or nil) or tree.
type tree = map[string]any

// localeFile parses a translation file name: LOCALE.json or SCOPE.LOCALE.json
// (such as posts.en.json), where LOCALE is canonical (see CanonicalLocale).
func localeFile(name string) (string, error) {
	base := strings.TrimSuffix(name, ".json")
	scope, locale := "", base
	if cut := strings.LastIndexByte(base, '.'); cut >= 0 {
		scope, locale = base[:cut], base[cut+1:]
	}
	canonical, err := CanonicalLocale(locale)
	if err != nil {
		return "", fmt.Errorf("i18n: %s: name translation files LOCALE.json or SCOPE.LOCALE.json, such as en.json or posts.pt-BR.json: %w", name, err)
	}
	if canonical != locale {
		return "", fmt.Errorf("i18n: %s: spell the locale %s (rename the file to %s)", name, canonical, strings.TrimSuffix(name, locale+".json")+canonical+".json")
	}
	if scope != "" && !scopePattern.MatchString(scope) {
		return "", fmt.Errorf("i18n: %s: the part before the locale must use lowercase letters, digits, '_', '-' and '.'", name)
	}
	return canonical, nil
}

// readCatalog reads every *.json file at the root of fsys (subdirectories
// and other files are ignored) and merges the files of each locale.
func readCatalog(fsys fs.FS) (map[string]tree, error) {
	if fsys == nil {
		return nil, errors.New("i18n: translation filesystem is required")
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("i18n: read translations: %w", err)
	}
	catalog := map[string]tree{}
	sources := map[string]map[string]string{} // locale -> leaf key -> file
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if files++; files > MaxFiles {
			return nil, fmt.Errorf("i18n: more than %d translation files", MaxFiles)
		}
		locale, err := localeFile(name)
		if err != nil {
			return nil, err
		}
		data, err := readLimited(fsys, name)
		if err != nil {
			return nil, err
		}
		parsed, err := parseFile(name, locale, data)
		if err != nil {
			return nil, err
		}
		if catalog[locale] == nil {
			catalog[locale], sources[locale] = tree{}, map[string]string{}
		}
		if err := merge(catalog[locale], parsed, "", name, sources[locale]); err != nil {
			return nil, err
		}
		if len(sources[locale]) > MaxKeysPerLocale {
			return nil, fmt.Errorf("i18n: locale %s has more than %d keys", locale, MaxKeysPerLocale)
		}
	}
	if len(catalog) == 0 {
		return nil, errors.New("i18n: no translation files found; expected LOCALE.json files such as en.json")
	}
	return catalog, nil
}

func readLimited(fsys fs.FS, name string) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("i18n: open %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("i18n: read %s: %w", name, err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("i18n: %s is larger than %d bytes", name, MaxFileBytes)
	}
	return data, nil
}

// parseFile decodes one file. Its only top-level key is the locale, like a
// Rails YAML file: {"en": {"users": {"welcome": "Welcome, %{name}!"}}}.
// Duplicate keys in an object are an error, not "last one wins".
func parseFile(name, locale string, data []byte) (tree, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root, err := parseValue(decoder, 0)
	if err != nil {
		return nil, fmt.Errorf("i18n: %s: %w", name, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("i18n: %s: unexpected data after the top-level object", name)
	}
	object, ok := root.(tree)
	if !ok || len(object) != 1 {
		return nil, fmt.Errorf("i18n: %s: the file must be one object whose only key is the locale, {%q: {...}}", name, locale)
	}
	translations, ok := object[locale].(tree)
	if !ok {
		return nil, fmt.Errorf("i18n: %s: the only top-level key must be %q (the locale in the file name) and hold an object", name, locale)
	}
	if err := checkTree(translations, ""); err != nil {
		return nil, fmt.Errorf("i18n: %s: %w", name, err)
	}
	return translations, nil
}

// parseValue reads one JSON value token by token, so duplicate keys and
// excessive nesting are caught.
func parseValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		if depth > MaxDepth {
			return nil, fmt.Errorf("nesting deeper than %d levels", MaxDepth)
		}
		switch value {
		case '{':
			object := tree{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("invalid JSON: %w", err)
				}
				key, _ := keyToken.(string)
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("duplicate key %q", truncate(key))
				}
				if object[key], err = parseValue(decoder, depth+1); err != nil {
					return nil, err
				}
			}
			_, err := decoder.Token() // }
			return object, err
		case '[':
			list := []any{}
			for decoder.More() {
				item, err := parseValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				list = append(list, item)
			}
			_, err := decoder.Token() // ]
			return list, err
		}
		return nil, fmt.Errorf("invalid JSON delimiter %q", value)
	default:
		return value, nil // string, json.Number, bool or nil
	}
}

// checkTree validates keys and values below a locale.
func checkTree(node tree, prefix string) error {
	allPlural := len(node) > 0
	for key, value := range node {
		path := joinKey(prefix, key)
		if !segmentPattern.MatchString(key) {
			return fmt.Errorf("key %q: use letters, digits, '_' and '-' (dots separate levels)", truncate(path))
		}
		if len(path) > MaxKeyLength {
			return fmt.Errorf("key %q is longer than %d bytes", truncate(path), MaxKeyLength)
		}
		allPlural = allPlural && isCategory(key)
		switch typed := value.(type) {
		case tree:
			if err := checkTree(typed, path); err != nil {
				return err
			}
		case []any:
			for _, item := range typed {
				if _, ok := item.(string); !ok && item != nil {
					return fmt.Errorf("key %q: lists may hold only strings and null", path)
				}
			}
		case nil:
			return fmt.Errorf("key %q is null", path)
		}
	}
	if allPlural {
		if _, ok := node[string(Other)]; !ok {
			return fmt.Errorf("key %q: plural forms need an %q form", prefix, Other)
		}
		for key, value := range node {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("key %q: plural form %q must be a string", prefix, key)
			}
		}
	}
	return nil
}

// isPlural reports whether an object holds plural forms.
func isPlural(node tree) bool {
	if _, ok := node[string(Other)]; !ok {
		return false
	}
	for key := range node {
		if !isCategory(key) {
			return false
		}
	}
	return true
}

// merge adds src to dst. A leaf defined by two files is an error naming both.
func merge(dst, src tree, prefix, file string, sources map[string]string) error {
	for _, key := range sortedKeys(src) {
		path := joinKey(prefix, key)
		value := src[key]
		existing, exists := dst[key]
		srcTree, srcIsTree := value.(tree)
		dstTree, dstIsTree := existing.(tree)
		switch {
		case !exists:
			dst[key] = value
			recordLeaves(value, path, file, sources)
		case srcIsTree && dstIsTree && !isPlural(srcTree) && !isPlural(dstTree):
			if err := merge(dstTree, srcTree, path, file, sources); err != nil {
				return err
			}
		default:
			return fmt.Errorf("i18n: key %q is defined in both %s and %s", path, firstSource(sources, path), file)
		}
	}
	return nil
}

func recordLeaves(value any, path, file string, sources map[string]string) {
	if node, ok := value.(tree); ok && !isPlural(node) {
		for key, child := range node {
			recordLeaves(child, joinKey(path, key), file, sources)
		}
		return
	}
	sources[path] = file
}

func firstSource(sources map[string]string, path string) string {
	if file, ok := sources[path]; ok {
		return file
	}
	for key, file := range sources {
		if strings.HasPrefix(key, path+".") || strings.HasPrefix(path, key+".") {
			return file
		}
	}
	return "another file"
}

// leafKeys lists the dotted keys of a tree's leaves, sorted. Plural forms are
// one leaf.
func leafKeys(node tree) []string {
	keys := appendLeafKeys(node, "", nil)
	slices.Sort(keys)
	return keys
}

func appendLeafKeys(node tree, prefix string, keys []string) []string {
	for key, value := range node {
		path := joinKey(prefix, key)
		if child, ok := value.(tree); ok && !isPlural(child) {
			keys = appendLeafKeys(child, path, keys)
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

// find walks a dotted key.
func find(node tree, key string) (any, bool) {
	var current any = node
	for segment := range strings.SplitSeq(key, ".") {
		object, ok := current.(tree)
		if !ok {
			return nil, false
		}
		if current, ok = object[segment]; !ok {
			return nil, false
		}
	}
	return current, true
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func sortedKeys(node tree) []string {
	keys := make([]string, 0, len(node))
	for key := range node {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
