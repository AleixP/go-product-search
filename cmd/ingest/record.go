package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// Slots of the required columns inside header.cols.
const (
	colCode = iota
	colName
	colBrands
	colCategories
	colCountries
	colNutriscore
	numCols
)

// colNames are the required column names, indexed by slot.
var colNames = [numCols]string{"code", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade"}

// utf8BOM is the byte order mark some exporters put at the start of a file.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

var (
	tabSep = []byte{'\t'}
	comma  = []byte{','}
	// catPrefix marks the English category tags; it is case sensitive.
	catPrefix = []byte("en:")
)

// header holds where the required columns are and how wide a row must be. It
// keeps only integers, so it never aliases the line it was parsed from.
type header struct {
	width int          // number of columns in the header
	cols  [numCols]int // column index of each required column, by slot
	last  int          // largest value in cols: fields after it are never read
}

// parseHeader finds the required columns by exact name. A UTF-8 BOM at the
// very start is dropped. With duplicate names the first occurrence wins.
// Missing columns are listed sorted; the error never echoes the header.
func parseHeader(line []byte) (header, error) {
	line = bytes.TrimPrefix(line, utf8BOM)
	if len(line) == 0 {
		return header{}, errors.New("empty header line")
	}
	h := header{width: bytes.Count(line, tabSep) + 1}
	for s := range h.cols {
		h.cols[s] = -1
	}
	for i, name := range bytes.Split(line, tabSep) {
		for s, want := range colNames {
			if h.cols[s] < 0 && string(name) == want {
				h.cols[s] = i
			}
		}
	}
	var missing []string
	for s, c := range h.cols {
		h.last = max(h.last, c)
		if c < 0 {
			missing = append(missing, colNames[s])
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return header{}, fmt.Errorf("missing required columns: %s", strings.Join(missing, ", "))
	}
	return h, nil
}

// skipReason says why a line did not pass. It is both what decode returns (the
// four decode reasons) and the bucket key of the report: the reasons after
// skipOversized are never returned by decode, they come from the line reader
// (skipTooLong) and from the filters (filter.go). One enum, one set of report
// keys, no duplicated names.
type skipReason uint8

const (
	skipNone skipReason = iota
	skipMalformed
	skipInvalidCode
	skipNoName
	skipOversized
	skipTooLong
	skipNotSampled
	skipCountry
	skipBrand
	skipCategory
	skipNutriscore
	numSkipReasons
)

// String returns the report key of the reason ("none" for skipNone).
func (r skipReason) String() string {
	switch r {
	case skipMalformed:
		return "malformed"
	case skipInvalidCode:
		return "invalid_code"
	case skipNoName:
		return "no_name"
	case skipOversized:
		return "oversized"
	case skipTooLong:
		return "too_long"
	case skipNotSampled:
		return "not_sampled"
	case skipCountry:
		return "country"
	case skipBrand:
		return "brand"
	case skipCategory:
		return "category"
	case skipNutriscore:
		return "nutriscore"
	}
	return "none"
}

// record is one decoded row. All strings are copies: nothing refers to the
// line it was decoded from.
type record struct {
	code, name string
	brands     []string
	categories []string
	nutriscore string
	countries  []string
}

// maxCodeBytes is the longest accepted product code (stored bytes). The code is
// the document _id, and OpenSearch refuses ids above 512 bytes.
const maxCodeBytes = 128

// Size caps on the raw (untrimmed) text of a field. A row over a cap is skipped
// whole as "oversized", never truncated: a product is not indexed with half of
// its data. They bound what one row can keep alive (the record, its JSON and
// the dedupe set) whatever the line size, which can reach 1 MiB (maxLineBytes).
//
// The real maxima over 196,962 rows are: name 255 bytes, brands 4 items,
// categories_tags 26 items, countries_tags 17 items, whole row 18,222 bytes.
//
// shortcut: a legitimate product with a name over 1 KiB or a list field over
// 16 KiB is lost (none in the real export, 4x and about 8x the maxima); raise
// the cap, or split the field, if the export ever grows to that.
//
// Worst case of one accepted row: name 1 KiB, brands and categories 16 KiB
// each in the document (countries are only kept in memory, 16 KiB too). JSON
// writes a control character as 6 bytes, so the document is at most about
// 6 x (1 + 16 + 16) KiB = 198 KiB. The Item 9 batch rule (512 KiB of field
// bytes + 100 bytes per document, 1,000 documents) bounds the batch; a control
// byte becomes 6 JSON bytes, so the bulk body can be about 6x the counted bytes
// (3 MiB at 512 KiB). Measured with a batch model of 1,000 worst-case records,
// over repeated runs: peak RSS 65-80 MB at a 2 MiB cap and 28-32 MB at 512 KiB
// (single runs vary by a few MB).
const (
	maxNameBytes      = 1 << 10  // product_name
	maxListFieldBytes = 16 << 10 // brands, categories_tags, countries_tags
)

// decode extracts the needed fields of one data line, without allocating the
// other columns: it counts the tabs once, then walks them up to the last
// needed column. A quote is ordinary data; the export never quotes fields.
//
// Checks, in this order, the first failure wins:
//  1. the field count must equal the header width (an empty line has one
//     field, so it is malformed too);
//  2. the code, trimmed with strings.TrimSpace semantics (all Unicode spaces),
//     must be non-empty, at most 128 bytes, valid UTF-8 (it is the document
//     id: distinct raw codes must never collapse into one U+FFFD id) and free
//     of control characters: C0 (below 0x20), DEL and C1 (U+0080 to U+009F),
//     checked per rune. Inner spaces are allowed; the stored code is the
//     trimmed one, unchanged;
//  3. the raw length of product_name (over maxNameBytes) and of brands,
//     categories_tags and countries_tags (each over maxListFieldBytes) must be
//     within the caps. It is measured before any trim, copy or split, so a
//     huge row is skipped without allocating anything;
//  4. the trimmed name must not be empty (a name of only spaces over the cap is
//     oversized, not no_name).
//
// Every string of the record except the code is a copy with invalid UTF-8
// replaced by U+FFFD, so the record stays valid after the reader reuses the
// line buffer. brands are trimmed values with case and inner spacing kept
// ("raw" means that, not byte-identical: the invalid bytes are replaced first),
// deduplicated after the replacement so the dedupe matches what is stored (two
// different invalid bytes give one "\uFFFD"); categories keep only the "en:"
// tags (case sensitive, after trimming) without the prefix, trimmed again,
// deduplicated; countries keep their "en:" prefix because the filter compares
// whole taxonomy tags, empties dropped, not deduplicated; the grade is "a" to
// "e" (any case) or "". Empty lists are nil.
func (h header) decode(line []byte) (record, skipReason) {
	if bytes.Count(line, tabSep)+1 != h.width {
		return record{}, skipMalformed
	}
	var f [numCols][]byte
	rest := line
	for i := 0; i <= h.last; i++ {
		field := rest
		if j := bytes.IndexByte(rest, '\t'); j >= 0 {
			field, rest = rest[:j], rest[j+1:]
		}
		for s, c := range h.cols {
			if c == i {
				f[s] = field
			}
		}
	}
	code := bytes.TrimSpace(f[colCode])
	if len(code) == 0 || len(code) > maxCodeBytes || !utf8.Valid(code) || hasControl(code) {
		return record{}, skipInvalidCode
	}
	if len(f[colName]) > maxNameBytes || len(f[colBrands]) > maxListFieldBytes ||
		len(f[colCategories]) > maxListFieldBytes || len(f[colCountries]) > maxListFieldBytes {
		return record{}, skipOversized
	}
	name := cleanString(f[colName])
	if name == "" {
		return record{}, skipNoName
	}
	return record{
		code:       string(code),
		name:       name,
		brands:     listField(f[colBrands], nil, true),
		categories: listField(f[colCategories], catPrefix, true),
		nutriscore: grade(f[colNutriscore]),
		countries:  listField(f[colCountries], nil, false),
	}, skipNone
}

// cleanString copies b, trims it and replaces invalid UTF-8.
func cleanString(b []byte) string {
	return strings.ToValidUTF8(string(bytes.TrimSpace(b)), "\uFFFD")
}

// hasControl reports whether the valid UTF-8 text b has an ASCII control
// character, DEL or a C1 control (U+0080 to U+009F). It checks runes, not
// bytes: the second byte of a rune such as U+0101 (C4 81) is not a control.
func hasControl(b []byte) bool {
	for _, r := range string(b) { // no copy: the compiler ranges over the bytes
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

// linearDedupe is how many items are compared one by one before a set is
// built: short lists (real rows) need no map, hostile ones must not be
// quadratic.
const linearDedupe = 32

// maxListCap caps the up-front capacity of a list: the item count of a line
// is untrusted, the real lists have a handful of items.
const maxListCap = 64

// listField splits b on commas. Each item is trimmed; with a prefix only items
// starting with it are kept, without it; empty items are dropped and, with
// unique, so are duplicates (exact match, the first position wins). Items are
// trimmed again after the prefix, copied, invalid UTF-8 replaced (the dedupe
// compares the replaced text). It returns nil when nothing is kept.
func listField(b, prefix []byte, unique bool) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	var seen map[string]struct{}
	for rest, more := b, true; more; {
		var item []byte
		if j := bytes.IndexByte(rest, ','); j >= 0 {
			item, rest = rest[:j], rest[j+1:]
		} else {
			item, more = rest, false
		}
		item, ok := bytes.CutPrefix(bytes.TrimSpace(item), prefix)
		item = bytes.TrimSpace(item)
		if !ok || len(item) == 0 {
			continue
		}
		s := strings.ToValidUTF8(string(item), "\uFFFD")
		if unique {
			if seen != nil {
				if _, dup := seen[s]; dup {
					continue
				}
				seen[s] = struct{}{}
			} else if slices.Contains(out, s) {
				continue
			}
		}
		if out == nil {
			out = make([]string, 0, min(bytes.Count(b, comma)+1, maxListCap))
		}
		out = append(out, s)
		if unique && seen == nil && len(out) == linearDedupe {
			seen = make(map[string]struct{}, 2*linearDedupe)
			for _, o := range out {
				seen[o] = struct{}{}
			}
		}
	}
	return out
}

// grade returns "a" to "e" for a trimmed, case-folded grade, "" otherwise.
func grade(b []byte) string {
	switch s := strings.ToLower(strings.TrimSpace(string(b))); s {
	case "a", "b", "c", "d", "e":
		return s
	}
	return ""
}

// product builds the document. Brands and Categories are never nil so the
// JSON is [] and not null.
func (r record) product() catalog.Product {
	p := catalog.Product{ID: r.code, Name: r.name, Brands: r.brands, Categories: r.categories, Nutriscore: r.nutriscore}
	if p.Brands == nil {
		p.Brands = []string{}
	}
	if p.Categories == nil {
		p.Categories = []string{}
	}
	return p
}
