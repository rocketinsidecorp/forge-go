package i18n_test

import (
	"slices"
	"testing"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

// TestPluralRules checks the built-in rules against CLDR 46 samples.
func TestPluralRules(t *testing.T) {
	t.Parallel()
	type sample struct {
		count any
		want  i18n.Category
	}
	o, f, tw, fw, m, z := i18n.One, i18n.Other, i18n.Two, i18n.Few, i18n.Many, i18n.Zero
	tables := map[string][]sample{
		"en":    {{0, f}, {1, o}, {2, f}, {-1, o}, {1.0, o}, {1.5, f}, {21, f}, {uint8(1), o}},
		"de":    {{1, o}, {0, f}, {1.5, f}},
		"es":    {{0, f}, {1, o}, {1.0, o}, {2, f}, {1_000_000, m}, {2_000_000, m}, {1_000_001, f}, {1.5, f}},
		"fr":    {{0, o}, {1, o}, {1.5, o}, {2, f}, {1_000_000, m}, {2.5, f}},
		"it":    {{0, f}, {1, o}, {1.5, f}, {1_000_000, m}},
		"pt":    {{0, o}, {1, o}, {1.5, o}, {2, f}, {1_000_000, m}},
		"pt-BR": {{0, o}, {1, o}, {2, f}},
		"pt-PT": {{0, f}, {1, o}, {1.5, f}, {2, f}, {1_000_000, m}},
		"ru": {
			{1, o}, {21, o}, {101, o}, {11, m}, {111, m}, {2, fw}, {3, fw}, {4, fw}, {22, fw}, {12, m}, {14, m},
			{0, m}, {5, m}, {10, m}, {25, m}, {100, m}, {1.5, f}, {-21, o},
		},
		"uk": {{1, o}, {2, fw}, {5, m}, {11, m}, {0.5, f}},
		"pl": {
			{1, o}, {2, fw}, {3, fw}, {4, fw}, {22, fw}, {12, m}, {13, m}, {14, m}, {0, m}, {5, m}, {21, m},
			{11, m}, {112, m}, {102, fw}, {1.5, f},
		},
		"cs": {{1, o}, {2, fw}, {4, fw}, {5, f}, {0, f}, {1.5, m}},
		"ar": {
			{0, z}, {1, o}, {2, tw}, {3, fw}, {10, fw}, {103, fw}, {110, fw}, {11, m}, {99, m}, {111, m},
			{100, f}, {101, f}, {102, f}, {1.5, f},
		},
		"he":      {{1, o}, {2, tw}, {3, f}, {0.5, o}, {0, f}},
		"tr":      {{1, o}, {1.0, o}, {2, f}, {0, f}},
		"ja":      {{0, f}, {1, f}, {2, f}},
		"zh":      {{1, f}},
		"zh-Hant": {{1, f}},
		"ko":      {{1, f}},
		"xx":      {{0, f}, {1, f}}, // unknown: always other
	}
	for locale, samples := range tables {
		rule := i18n.PluralRuleFor(locale)
		for _, s := range samples {
			if got := rule(operands(s.count)); got != s.want {
				t.Errorf("%s: count %v = %s, want %s", locale, s.count, got, s.want)
			}
		}
	}
}

func operands(count any) i18n.Operands {
	result, ok := i18n.NewOperands(count)
	if !ok {
		panic("not a number")
	}
	return result
}

func TestPluralCategoriesTranslatorsMustProvide(t *testing.T) {
	t.Parallel()
	translator := load(t, i18n.Options{})
	want := map[string][]i18n.Category{
		"en": {i18n.One, i18n.Other},
		"pt": {i18n.One, i18n.Other},
		"es": {i18n.One, i18n.Other}, // whole millions fall back to other
		"ru": {i18n.One, i18n.Few, i18n.Many},
		"pl": {i18n.One, i18n.Few, i18n.Many},
		"ar": {i18n.Zero, i18n.One, i18n.Two, i18n.Few, i18n.Many, i18n.Other},
		"ja": {i18n.Other},
	}
	for locale, categories := range want {
		if got := translator.PluralCategories(locale); !slices.Equal(got, categories) {
			t.Errorf("PluralCategories(%s) = %v, want %v", locale, got, categories)
		}
	}
}
