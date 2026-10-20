package i18n

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// builtinFormats are the English formats used when neither the locale, its
// parents nor the default locale define a formatting key (the defaults of
// Rails' en locale).
var builtinFormats = tree{
	"number": tree{"format": tree{"separator": ".", "delimiter": ",", "precision": json.Number("2")}},
	"date": tree{
		"formats":          tree{"default": "%Y-%m-%d", "short": "%b %d", "long": "%B %d, %Y"},
		"day_names":        []any{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
		"abbr_day_names":   []any{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
		"month_names":      []any{nil, "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
		"abbr_month_names": []any{nil, "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	},
	"time": tree{
		"formats": tree{"default": "%a, %d %b %Y %H:%M:%S %z", "short": "%d %b %H:%M", "long": "%B %d, %Y %H:%M"},
		"am":      "am",
		"pm":      "pm",
	},
}

// data finds a formatting value in the locale, its parents, the default
// locale and its parents, then the built-in English formats. Formatting data
// always falls back, in every environment.
func (t *Translator) data(locale, key string) (any, bool) {
	for _, chain := range [][]string{parents(locale), parents(t.defaultLocale)} {
		for _, candidate := range chain {
			if node, ok := t.catalog[candidate]; ok {
				if value, ok := find(node, key); ok {
					return value, true
				}
			}
		}
	}
	return find(builtinFormats, key)
}

func (t *Translator) dataString(locale, key string) string {
	value, _ := t.data(locale, key)
	text, _ := value.(string)
	return text
}

func (t *Translator) dataName(locale, key string, index int) string {
	value, _ := t.data(locale, key)
	if list, ok := value.([]any); ok && index >= 0 && index < len(list) {
		if name, ok := list[index].(string); ok {
			return name
		}
	}
	builtin, _ := find(builtinFormats, key)
	list, _ := builtin.([]any)
	if index >= 0 && index < len(list) {
		name, _ := list[index].(string)
		return name
	}
	return ""
}

// FormatNumber writes value with the locale's number.format.separator and
// number.format.delimiter (thousands grouping). Integers get no decimals
// unless precision is positive; floats get precision decimals, or
// number.format.precision when precision is negative. ok is false for
// values that are not numbers, which are returned with fmt.Sprint.
func (t *Translator) FormatNumber(locale string, value any, precision int) (string, bool) {
	locale = t.Match(locale)
	if precision > 20 {
		precision = 20
	}
	var digits string
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		digits = fmt.Sprint(typed)
		if precision > 0 {
			digits += "." + strings.Repeat("0", precision)
		}
	case float32, float64:
		number := toFloat(typed)
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return strconv.FormatFloat(number, 'f', -1, 64), true
		}
		if precision < 0 {
			precision = t.defaultPrecision(locale)
		}
		digits = strconv.FormatFloat(number, 'f', precision, 64)
	default:
		return fmt.Sprint(value), false
	}
	return t.groupDigits(locale, digits), true
}

func toFloat(value any) float64 {
	if small, ok := value.(float32); ok {
		return float64(small)
	}
	number, _ := value.(float64)
	return number
}

func (t *Translator) defaultPrecision(locale string) int {
	value, _ := t.data(locale, "number.format.precision")
	if number, ok := value.(json.Number); ok {
		if precision, err := strconv.Atoi(number.String()); err == nil && precision >= 0 && precision <= 20 {
			return precision
		}
	}
	return 2
}

// groupDigits localizes "-1234567.50" as "-1,234,567.50" (en) or
// "-1.234.567,50" (pt-BR).
func (t *Translator) groupDigits(locale, digits string) string {
	separator := t.dataString(locale, "number.format.separator")
	delimiter := t.dataString(locale, "number.format.delimiter")
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	whole, fraction, hasFraction := strings.Cut(digits, ".")
	var out strings.Builder
	out.WriteString(sign)
	for i := range len(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out.WriteString(delimiter)
		}
		out.WriteByte(whole[i])
	}
	if hasFraction {
		out.WriteString(separator)
		out.WriteString(fraction)
	}
	return out.String()
}

// FormatTime formats value with a strftime pattern from the locale:
// format "short" uses time.formats.short (empty means "default"), and a
// format containing a dot is a full key such as "date.formats.long". The
// directives are %Y %y %m %d %e %H %I %M %S %p %P %A %a %B %b %z %Z %j and %%
// ("%-d" removes padding); day and month names come from date.day_names,
// date.abbr_day_names, date.month_names and date.abbr_month_names, and %p from
// time.am and time.pm.
func (t *Translator) FormatTime(locale string, value time.Time, format string) string {
	locale = t.Match(locale)
	if format == "" {
		format = "default"
	}
	key := format
	if !strings.Contains(format, ".") {
		key = "time.formats." + format
	}
	pattern := t.dataString(locale, key)
	if pattern == "" {
		pattern = t.dataString(locale, "time.formats.default")
	}
	return t.strftime(locale, value, pattern)
}

func (t *Translator) strftime(locale string, value time.Time, pattern string) string {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 == len(pattern) {
			out.WriteByte(pattern[i])
			continue
		}
		i++
		pad := true
		if pattern[i] == '-' && i+1 < len(pattern) {
			pad = false
			i++
		}
		number := func(n, width int) {
			text := strconv.Itoa(n)
			if pad && len(text) < width {
				text = strings.Repeat("0", width-len(text)) + text
			}
			out.WriteString(text)
		}
		switch pattern[i] {
		case 'Y':
			out.WriteString(strconv.Itoa(value.Year()))
		case 'y':
			number(value.Year()%100, 2)
		case 'm':
			number(int(value.Month()), 2)
		case 'd':
			number(value.Day(), 2)
		case 'e':
			if day := value.Day(); pad && day < 10 {
				out.WriteString(" " + strconv.Itoa(day))
			} else {
				out.WriteString(strconv.Itoa(day))
			}
		case 'j':
			number(value.YearDay(), 3)
		case 'H':
			number(value.Hour(), 2)
		case 'I':
			hour := value.Hour() % 12
			if hour == 0 {
				hour = 12
			}
			number(hour, 2)
		case 'M':
			number(value.Minute(), 2)
		case 'S':
			number(value.Second(), 2)
		case 'p', 'P':
			marker := t.dataString(locale, "time.am")
			if value.Hour() >= 12 {
				marker = t.dataString(locale, "time.pm")
			}
			if pattern[i] == 'p' {
				marker = strings.ToUpper(marker)
			} else {
				marker = strings.ToLower(marker)
			}
			out.WriteString(marker)
		case 'A':
			out.WriteString(t.dataName(locale, "date.day_names", int(value.Weekday())))
		case 'a':
			out.WriteString(t.dataName(locale, "date.abbr_day_names", int(value.Weekday())))
		case 'B':
			out.WriteString(t.dataName(locale, "date.month_names", int(value.Month())))
		case 'b':
			out.WriteString(t.dataName(locale, "date.abbr_month_names", int(value.Month())))
		case 'z':
			out.WriteString(value.Format("-0700"))
		case 'Z':
			out.WriteString(value.Format("MST"))
		case '%':
			out.WriteByte('%')
		default:
			out.WriteByte('%')
			if !pad {
				out.WriteByte('-')
			}
			out.WriteByte(pattern[i])
		}
	}
	return out.String()
}

// Number formats value for the locale of ctx (see Translator.FormatNumber);
// without a translator it uses fmt.Sprint.
func Number(ctx context.Context, value any, precision int) string {
	t, locale, ok := FromContext(ctx)
	if !ok {
		return fmt.Sprint(value)
	}
	text, _ := t.FormatNumber(locale, value, precision)
	return text
}

// Localize formats a time for the locale of ctx (see Translator.FormatTime);
// without a translator it uses RFC 3339.
func Localize(ctx context.Context, value time.Time, format string) string {
	t, locale, ok := FromContext(ctx)
	if !ok {
		return value.Format(time.RFC3339)
	}
	return t.FormatTime(locale, value, format)
}
