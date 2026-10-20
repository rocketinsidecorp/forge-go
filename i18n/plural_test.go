package i18n_test

import (
	"math"
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
		"nb":      {{1, o}, {1.0, o}, {0, f}, {2, f}, {1.5, f}},
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

// TestPluralRulesDecimalSamples checks the CLDR 46 decimal samples with
// visible fraction digits ("1.0", "0.00"), which a float64 cannot carry, so
// the operands are written out: n, the integer digits i and the number of
// visible fraction digits v.
func TestPluralRulesDecimalSamples(t *testing.T) {
	t.Parallel()
	dec := func(n float64, v int) i18n.Operands { return i18n.Operands{N: n, I: int64(n), V: v} }
	type sample struct {
		text string
		o    i18n.Operands
		want i18n.Category
	}
	z, o, tw, fw, m, f := i18n.Zero, i18n.One, i18n.Two, i18n.Few, i18n.Many, i18n.Other
	tables := map[string][]sample{
		// ar: zero n = 0; one n = 1; two n = 2; few n % 100 = 3..10;
		// many n % 100 = 11..99.
		"ar": {
			{"0", dec(0, 0), z}, {"0.0", dec(0, 1), z}, {"0.00", dec(0, 2), z},
			{"1", dec(1, 0), o}, {"1.0", dec(1, 1), o}, {"1.00", dec(1, 2), o},
			{"2", dec(2, 0), tw}, {"2.0", dec(2, 1), tw}, {"2.00", dec(2, 2), tw},
			{"3", dec(3, 0), fw}, {"3.0", dec(3, 1), fw}, {"10.0", dec(10, 1), fw}, {"103.0", dec(103, 1), fw},
			{"110.00", dec(110, 2), fw}, {"1003.0", dec(1003, 1), fw},
			{"11", dec(11, 0), m}, {"11.0", dec(11, 1), m}, {"26.00", dec(26, 2), m}, {"99.0", dec(99, 1), m},
			{"111.0", dec(111, 1), m}, {"1011.0", dec(1011, 1), m},
			{"100", dec(100, 0), f}, {"100.0", dec(100, 1), f}, {"101.0", dec(101, 1), f}, {"102.00", dec(102, 2), f},
			{"200.0", dec(200, 1), f}, {"1000.0", dec(1000, 1), f},
			{"0.1", dec(0.1, 1), f}, {"0.9", dec(0.9, 1), f}, {"1.1", dec(1.1, 1), f}, {"1.7", dec(1.7, 1), f},
			{"2.5", dec(2.5, 1), f}, {"3.5", dec(3.5, 1), f}, {"10.1", dec(10.1, 1), f}, {"11.5", dec(11.5, 1), f},
			{"100.1", dec(100.1, 1), f}, {"103.5", dec(103.5, 1), f}, {"1000.1", dec(1000.1, 1), f},
		},
		// n = 1 languages: "1.0" is singular, unlike the "i = 1 and v = 0" ones.
		"nb": {{"1.0", dec(1, 1), o}, {"1.00", dec(1, 2), o}, {"0.0", dec(0, 1), f}, {"1.5", dec(1.5, 1), f}},
		"tr": {{"1.0", dec(1, 1), o}, {"1.5", dec(1.5, 1), f}},
		"el": {{"1.0", dec(1, 1), o}, {"2.0", dec(2, 1), f}},
		"hu": {{"1.0", dec(1, 1), o}, {"0.5", dec(0.5, 1), f}},
		"es": {{"1.0", dec(1, 1), o}, {"1.5", dec(1.5, 1), f}, {"1000000.0", dec(1e6, 1), f}},
		// i = 1 and v = 0 languages: "1.0" is plural.
		"en": {{"1.0", dec(1, 1), f}, {"1.00", dec(1, 2), f}, {"0.0", dec(0, 1), f}},
		"de": {{"1.0", dec(1, 1), f}},
		"nl": {{"1.0", dec(1, 1), f}},
		"sv": {{"1.0", dec(1, 1), f}},
		"fi": {{"1.0", dec(1, 1), f}},
		"et": {{"1.0", dec(1, 1), f}},
		"it": {{"1.0", dec(1, 1), f}, {"1000000.0", dec(1e6, 1), f}},
		// i = 0..1 languages.
		"fr": {{"0.0", dec(0, 1), o}, {"1.0", dec(1, 1), o}, {"1.5", dec(1.5, 1), o}, {"2.0", dec(2, 1), f}},
		"pt": {{"0.0", dec(0, 1), o}, {"1.0", dec(1, 1), o}, {"2.0", dec(2, 1), f}},
		// v = 0 only: decimals are "other".
		"ru": {{"1.0", dec(1, 1), f}, {"2.0", dec(2, 1), f}, {"5.0", dec(5, 1), f}},
		"uk": {{"1.0", dec(1, 1), f}},
		"pl": {{"1.0", dec(1, 1), f}, {"2.0", dec(2, 1), f}},
		"cs": {{"1.0", dec(1, 1), m}, {"2.0", dec(2, 1), m}, {"0.5", dec(0.5, 1), m}},
		"sk": {{"1.0", dec(1, 1), m}},
		// he: i = 1 and v = 0 or i = 0 and v != 0; i = 2 and v = 0.
		"he": {{"1.0", dec(1, 1), f}, {"2.0", dec(2, 1), f}, {"0.0", dec(0, 1), o}, {"0.5", dec(0.5, 1), o}},
	}
	for locale, samples := range tables {
		rule := i18n.PluralRuleFor(locale)
		for _, s := range samples {
			if got := rule(s.o); got != s.want {
				t.Errorf("%s: %s = %s, want %s", locale, s.text, got, s.want)
			}
		}
	}
}

// TestArabicNotANumber: NaN and infinity must not be zero, one or two.
func TestArabicNotANumber(t *testing.T) {
	t.Parallel()
	rule := i18n.PluralRuleFor("ar")
	for _, count := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := rule(operands(count)); got != i18n.Other {
			t.Errorf("ar: count %v = %s, want other", count, got)
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
