package i18n

import (
	"math"
	"strconv"
	"strings"
)

// Category is a CLDR plural category.
type Category string

// The CLDR plural categories. Every language uses Other; most use a few of
// the rest.
const (
	Zero  Category = "zero"
	One   Category = "one"
	Two   Category = "two"
	Few   Category = "few"
	Many  Category = "many"
	Other Category = "other"
)

var categories = []Category{Zero, One, Two, Few, Many, Other}

func isCategory(key string) bool {
	for _, category := range categories {
		if string(category) == key {
			return true
		}
	}
	return false
}

// Operands are the CLDR plural operands of a count: N is its absolute value,
// I its integer digits and V the number of visible fraction digits (0 for
// integers). Counts beyond the int64 range saturate.
type Operands struct {
	N float64
	I int64
	V int
}

// PluralRule chooses the plural category of a count. Rules for the
// languages Forge ships are built in (see PluralRuleFor); add others with
// Options.PluralRules, transcribing the language's rule from the CLDR plural
// rules chart (https://cldr.unicode.org/index/cldr-spec/plural-rules).
type PluralRule func(Operands) Category

// NewOperands converts a count: any integer or float type. ok is false for
// values that are not numbers.
func NewOperands(count any) (Operands, bool) {
	switch value := count.(type) {
	case int:
		return integerOperands(int64(value)), true
	case int8:
		return integerOperands(int64(value)), true
	case int16:
		return integerOperands(int64(value)), true
	case int32:
		return integerOperands(int64(value)), true
	case int64:
		return integerOperands(value), true
	case uint:
		return unsignedOperands(uint64(value)), true
	case uint8:
		return unsignedOperands(uint64(value)), true
	case uint16:
		return unsignedOperands(uint64(value)), true
	case uint32:
		return unsignedOperands(uint64(value)), true
	case uint64:
		return unsignedOperands(value), true
	case float32:
		return floatOperands(float64(value)), true
	case float64:
		return floatOperands(value), true
	}
	return Operands{}, false
}

func integerOperands(value int64) Operands {
	if value < 0 {
		if value == math.MinInt64 {
			value = math.MaxInt64
		} else {
			value = -value
		}
	}
	return Operands{N: float64(value), I: value}
}

func unsignedOperands(value uint64) Operands {
	if value > math.MaxInt64 {
		value = math.MaxInt64
	}
	return integerOperands(int64(value))
}

func floatOperands(value float64) Operands {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Operands{N: math.NaN(), I: 0, V: 1} // no rule on n counts it as 0, 1 or 2
	}
	value = math.Abs(value)
	text := strconv.FormatFloat(value, 'f', -1, 64)
	visible := 0
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		visible = len(text) - dot - 1
	}
	integer := int64(math.MaxInt64)
	if value < math.MaxInt64 {
		integer = int64(value)
	}
	return Operands{N: value, I: integer, V: visible}
}

// is reports whether the count is the integer n (CLDR "n = x").
func (o Operands) is(n int64) bool { return o.V == 0 && o.I == n }

// millions is the CLDR "many" condition of French, Spanish, Italian and
// Portuguese: a whole, non-zero number of millions ("1 million de ...").
func (o Operands) millions() bool { return o.V == 0 && o.I != 0 && o.I%1_000_000 == 0 }

// slavic is the one/few/many split of Russian and Ukrainian.
func slavic(o Operands) Category {
	if o.V != 0 {
		return Other
	}
	mod10, mod100 := o.I%10, o.I%100
	switch {
	case mod10 == 1 && mod100 != 11:
		return One
	case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
		return Few
	default:
		return Many
	}
}

func onlyOther(Operands) Category { return Other }

// oneIfIntegerOne is the rule of English, German, Dutch and the Nordic
// languages: "1 item", "0 items", "1.0 items".
func oneIfIntegerOne(o Operands) Category {
	if o.I == 1 && o.V == 0 {
		return One
	}
	return Other
}

// oneIfExactlyOne is "n = 1": the rule of Norwegian, Turkish, Greek and
// Hungarian ("1.0" is singular too).
func oneIfExactlyOne(o Operands) Category {
	if o.N == 1 {
		return One
	}
	return Other
}

// builtinRules are CLDR 46 plural rules for cardinal numbers.
var builtinRules = map[string]PluralRule{
	"en": oneIfIntegerOne,
	"de": oneIfIntegerOne,
	"nl": oneIfIntegerOne,
	"sv": oneIfIntegerOne,
	"nb": oneIfExactlyOne,
	"fi": oneIfIntegerOne,
	"et": oneIfIntegerOne,
	"tr": oneIfExactlyOne,
	"el": oneIfExactlyOne,
	"hu": oneIfExactlyOne,
	"ja": onlyOther,
	"zh": onlyOther,
	"ko": onlyOther,
	"vi": onlyOther,
	"th": onlyOther,
	"id": onlyOther,
	"ms": onlyOther,
	"es": func(o Operands) Category {
		switch {
		case o.N == 1:
			return One
		case o.millions():
			return Many
		}
		return Other
	},
	"it": func(o Operands) Category {
		switch {
		case o.I == 1 && o.V == 0:
			return One
		case o.millions():
			return Many
		}
		return Other
	},
	"fr": func(o Operands) Category {
		switch {
		case o.I == 0 || o.I == 1:
			return One
		case o.millions():
			return Many
		}
		return Other
	},
	// "pt" is Brazilian Portuguese in CLDR: 0 and 1 are singular.
	"pt": func(o Operands) Category {
		switch {
		case o.I == 0 || o.I == 1:
			return One
		case o.millions():
			return Many
		}
		return Other
	},
	// European Portuguese: only 1 is singular.
	"pt-PT": func(o Operands) Category {
		switch {
		case o.I == 1 && o.V == 0:
			return One
		case o.millions():
			return Many
		}
		return Other
	},
	"ru": slavic,
	"uk": slavic,
	"pl": func(o Operands) Category {
		if o.V != 0 {
			return Other
		}
		mod10, mod100 := o.I%10, o.I%100
		switch {
		case o.I == 1:
			return One
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return Few
		default:
			return Many
		}
	},
	"cs": czech,
	"sk": czech,
	"ar": arabic,
	"he": func(o Operands) Category {
		switch {
		case o.is(1) || (o.I == 0 && o.V != 0):
			return One
		case o.is(2):
			return Two
		}
		return Other
	},
}

// arabic is CLDR 46's rule, written in terms of n (the whole value, so "0.0",
// "1.0" and "103.0" count as integers) and not of the integer digits: zero
// n = 0, one n = 1, two n = 2, few n % 100 = 3..10, many n % 100 = 11..99.
// A range matches whole numbers only, so 3.5 is "other".
func arabic(o Operands) Category {
	if o.N != math.Trunc(o.N) { // also NaN
		return Other
	}
	switch mod100 := math.Mod(o.N, 100); {
	case o.N == 0:
		return Zero
	case o.N == 1:
		return One
	case o.N == 2:
		return Two
	case mod100 >= 3 && mod100 <= 10:
		return Few
	case mod100 >= 11 && mod100 <= 99:
		return Many
	}
	return Other
}

func czech(o Operands) Category {
	switch {
	case o.V != 0:
		return Many
	case o.I == 1:
		return One
	case o.I >= 2 && o.I <= 4:
		return Few
	}
	return Other
}

// PluralRuleFor returns the built-in rule of a canonical locale, trying its
// parents ("pt-BR" uses "pt", "pt-PT" has its own). Locales without one use
// Other for every count.
func PluralRuleFor(locale string) PluralRule {
	return ruleIn(nil, locale)
}

func ruleIn(custom map[string]PluralRule, locale string) PluralRule {
	for _, candidate := range parents(locale) {
		if rule, ok := custom[candidate]; ok {
			return rule
		}
		if rule, ok := builtinRules[candidate]; ok {
			return rule
		}
	}
	return onlyOther
}

// usedCategories lists the categories a rule gives the integers 0 to 1000,
// the forms a translator must provide. The "many" of whole millions (French,
// Spanish, Italian, Portuguese) is left out: it falls back to Other.
func usedCategories(rule PluralRule) []Category {
	seen := map[Category]bool{}
	for n := range int64(1001) {
		seen[rule(integerOperands(n))] = true
	}
	var used []Category
	for _, category := range categories {
		if seen[category] {
			used = append(used, category)
		}
	}
	return used
}
