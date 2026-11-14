package httpapi

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	maxQueryRunes = 100
	defaultPage   = 1
	defaultSize   = 20
	maxSize       = 100

	maxFilterValues = 20
	maxFilterRunes  = 256

	allowedSuffix = "; allowed parameters are q, page, size, brands, categories and nutriscore"
)

// safeKey limits which unknown keys may be named in an error message.
var safeKey = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]]{1,32}$`)

// parseSearchParams validates the raw query string of GET /search. The first
// error found wins: malformed query string, keys (checkKeys), q, page, size,
// the window, then brands, categories and nutriscore.
func parseSearchParams(rawQuery string) (catalog.Query, *apiError) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return catalog.Query{}, invalidParameter("malformed query string")
	}

	if aerr := checkKeys(values); aerr != nil {
		return catalog.Query{}, aerr
	}

	text, aerr := parseText("q", values.Get("q"), maxQueryRunes)
	if aerr != nil {
		return catalog.Query{}, aerr
	}

	page, ok := parseInt(values.Get("page"), defaultPage)
	if !ok || page < 1 {
		return catalog.Query{}, invalidParameter("page must be an integer >= 1")
	}

	size, ok := parseInt(values.Get("size"), defaultSize)
	if !ok || size < 1 || size > maxSize {
		return catalog.Query{}, invalidParameter("size must be an integer between 1 and 100")
	}

	// Division instead of page*size: no overflow for any page value.
	if last := catalog.MaxWindow / size; page > last {
		return catalog.Query{}, pageOutOfRange("page must be at most " + strconv.FormatInt(last, 10) + " when size is " + strconv.FormatInt(size, 10))
	}

	q := catalog.Query{Text: text, Page: int(page), Size: int(size)}
	if q.Brands, aerr = parseFilter("brands", values["brands"]); aerr != nil {
		return catalog.Query{}, aerr
	}
	if q.Categories, aerr = parseFilter("categories", values["categories"]); aerr != nil {
		return catalog.Query{}, aerr
	}
	if q.Nutriscore, aerr = parseNutriscore(values["nutriscore"]); aerr != nil {
		return catalog.Query{}, aerr
	}
	return q, nil
}

// checkKeys walks the keys in sorted order: unknown key, then repeated q, page
// or size, then too many occurrences of a filter key.
func checkKeys(values url.Values) *apiError {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		n := len(values[k])
		switch k {
		case "q", "page", "size":
			if n > 1 {
				return invalidParameter("parameter " + k + " must appear at most once")
			}
		case "brands", "categories", "nutriscore":
			if n > maxFilterValues {
				return invalidParameter(k + " accepts at most 20 values")
			}
		default:
			if safeKey.MatchString(k) {
				return invalidParameter("parameter \"" + k + "\" is not allowed" + allowedSuffix)
			}
			return invalidParameter("unknown parameter" + allowedSuffix)
		}
	}
	return nil
}

// parseFilter validates a free-text filter: trimmed, empties skipped, then
// deduplicated exactly and sorted. nil when nothing is left.
func parseFilter(name string, raw []string) ([]string, *apiError) {
	var out []string
	for _, v := range raw {
		v, aerr := parseText(name+" values", v, maxFilterRunes)
		if aerr != nil {
			return nil, aerr
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return sortedUnique(out), nil
}

// parseNutriscore trims the grades, skips empty ones, rejects anything but a
// single lowercase Nutri-Score grade (a to e), and returns them sorted
// and deduplicated (nil when none remain).
func parseNutriscore(raw []string) ([]string, *apiError) {
	var out []string
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) != 1 || !strings.Contains(catalog.NutriscoreGrades, v) {
			return nil, invalidParameter("nutriscore values must be one of a, b, c, d, e")
		}
		out = append(out, v)
	}
	return sortedUnique(out), nil
}

// sortedUnique sorts s in place and returns it without duplicates; it returns
// nil for an empty input.
func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	slices.Sort(s)
	return slices.Compact(s)
}

// parseText validates and trims one text value. what names the parameter in
// the messages ("q" or "brands values"); the input is never echoed.
func parseText(what, s string, maxRunes int) (string, *apiError) {
	if !utf8.ValidString(s) {
		return "", invalidParameter(what + " must be valid UTF-8")
	}
	s = strings.TrimSpace(s)
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", invalidParameter(what + " must not contain control characters")
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return "", invalidParameter(what + " must be at most " + strconv.Itoa(maxRunes) + " characters")
	}
	return s, nil
}

// parseInt returns def for an empty string. Otherwise s must be ASCII digits
// only (no sign, no spaces) and fit in an int64.
func parseInt(s string, def int64) (int64, bool) {
	if s == "" {
		return def, true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}
