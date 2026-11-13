package httpapi

import (
	"net/url"
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
)

// parseSearchParams validates the raw query string of GET /search. The first
// error found wins; see the order in the numbered steps below.
func parseSearchParams(rawQuery string) (catalog.Query, *apiError) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return catalog.Query{}, invalidParameter("malformed query string")
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k != "q" && k != "page" && k != "size" {
			return catalog.Query{}, invalidParameter("unknown parameter; allowed parameters are q, page and size")
		}
		if len(values[k]) > 1 {
			return catalog.Query{}, invalidParameter("parameter " + k + " must appear at most once")
		}
	}

	text, aerr := parseText(values.Get("q"))
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

	return catalog.Query{Text: text, Page: int(page), Size: int(size)}, nil
}

func parseText(s string) (string, *apiError) {
	if !utf8.ValidString(s) {
		return "", invalidParameter("q must be valid UTF-8")
	}
	s = strings.TrimSpace(s)
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", invalidParameter("q must not contain control characters")
	}
	if utf8.RuneCountInString(s) > maxQueryRunes {
		return "", invalidParameter("q must be at most 100 characters")
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
