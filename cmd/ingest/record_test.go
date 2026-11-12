package main

import (
	"reflect"
	"strings"
	"testing"
)

// recRequired lists the required columns in a fixed order used by the tests.
var recRequired = []string{"code", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade"}

// recValues are valid values for each required column.
var recValues = map[string]string{
	"code":             "123",
	"product_name":     "Name",
	"brands":           "B1,B2",
	"categories_tags":  "en:c1,fr:c2",
	"countries_tags":   "en:united-states",
	"nutriscore_grade": "a",
}

// recWant is the record that recValues decodes to.
var recWant = record{
	code:       "123",
	name:       "Name",
	brands:     []string{"B1", "B2"},
	categories: []string{"c1"},
	nutriscore: "a",
	countries:  []string{"en:united-states"},
}

// recJoin joins fields with tabs.
func recJoin(fields ...string) string { return strings.Join(fields, "\t") }

// recLineFor builds a data line for a header: required columns take
// recValues, any other column takes "x".
func recLineFor(cols []string) string {
	vals := make([]string, len(cols))
	for i, c := range cols {
		if v, ok := recValues[c]; ok {
			vals[i] = v
		} else {
			vals[i] = "x"
		}
	}
	return recJoin(vals...)
}

// recMustHeader parses a header line or fails the test.
func recMustHeader(t *testing.T, line string) header {
	t.Helper()
	h, err := parseHeader([]byte(line))
	if err != nil {
		t.Fatalf("parseHeader(%q) error = %v", line, err)
	}
	return h
}

func TestParseHeader_FindsColumnsByNameInAnyOrder(t *testing.T) {
	tests := []struct {
		name string
		cols []string
	}{
		{"fixed order", recRequired},
		{"reversed", []string{"nutriscore_grade", "countries_tags", "categories_tags", "brands", "product_name", "code"}},
		{"extra columns around", []string{"url", "nutriscore_grade", "brands", "code", "quantity", "categories_tags", "product_name", "countries_tags"}},
		{"extra column first and last", []string{"x1", "code", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade", "x2"}},
		{"similar names are not the names", []string{"brands_tags", "code", "product_name_en", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade", "categories"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := recMustHeader(t, recJoin(tt.cols...))
			if h.width != len(tt.cols) {
				t.Errorf("width = %d, want %d", h.width, len(tt.cols))
			}

			got, reason := h.decode([]byte(recLineFor(tt.cols)))

			if reason != skipNone {
				t.Fatalf("decode reason = %v, want none", reason)
			}
			if !reflect.DeepEqual(got, recWant) {
				t.Errorf("decode = %+v, want %+v", got, recWant)
			}
		})
	}
}

func TestParseHeader_StripsBOMAtTheStartOnly(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	h := recMustHeader(t, bom+recJoin(recRequired...))
	if h.width != len(recRequired) {
		t.Errorf("width = %d, want %d", h.width, len(recRequired))
	}
	got, reason := h.decode([]byte(recLineFor(recRequired)))
	if reason != skipNone || !reflect.DeepEqual(got, recWant) {
		t.Errorf("decode = %+v, %v; want %+v, none", got, reason, recWant)
	}

	// A BOM anywhere else is part of the column name.
	_, err := parseHeader([]byte("code\t" + bom + "product_name\tbrands\tcategories_tags\tcountries_tags\tnutriscore_grade"))
	if err == nil || err.Error() != "missing required columns: product_name" {
		t.Errorf("BOM inside the header: err = %v, want missing product_name", err)
	}
}

// Only ONE BOM is a byte order mark: a second one belongs to the first column
// name, so that column is no longer "code".
func TestParseHeader_OnlyOneBOMIsStripped(t *testing.T) {
	const bom = "\xef\xbb\xbf"

	_, err := parseHeader([]byte(bom + bom + recJoin(recRequired...)))

	if err == nil || err.Error() != "missing required columns: code" {
		t.Errorf("two BOMs: err = %v, want %q", err, "missing required columns: code")
	}
}

// A header of spaces is not empty: it is a one-column header with no required
// name, and the error says so (the "empty" error is only for zero bytes).
func TestParseHeader_SpacesOnlyIsNotEmpty(t *testing.T) {
	_, err := parseHeader([]byte("   "))

	const want = "missing required columns: brands, categories_tags, code, countries_tags, nutriscore_grade, product_name"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestParseHeader_MissingColumnsAreListedSorted(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"brands and code", recJoin("product_name", "categories_tags", "countries_tags", "nutriscore_grade", "url"),
			"missing required columns: brands, code"},
		{"one", recJoin("code", "product_name", "brands", "categories_tags", "countries_tags"),
			"missing required columns: nutriscore_grade"},
		{"all", recJoin("url", "quantity"),
			"missing required columns: brands, categories_tags, code, countries_tags, nutriscore_grade, product_name"},
		{"names are exact and case sensitive", recJoin("Code", " product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade"),
			"missing required columns: code, product_name"},
		{"BOM only", "\xef\xbb\xbf" + recJoin("brands", "product_name", "categories_tags", "countries_tags", "nutriscore_grade"),
			"missing required columns: code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseHeader([]byte(tt.line))

			if err == nil || err.Error() != tt.want {
				t.Errorf("parseHeader error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseHeader_EmptyHeaderIsAnError(t *testing.T) {
	for _, line := range []string{"", "\xef\xbb\xbf"} {
		_, err := parseHeader([]byte(line))
		if err == nil || err.Error() != "empty header line" {
			t.Errorf("parseHeader(%q) error = %v, want %q", line, err, "empty header line")
		}
	}
}

func TestParseHeader_ErrorsDoNotEchoTheHeader(t *testing.T) {
	_, err := parseHeader([]byte("secret-column\ttoken=hunter2"))
	if err == nil {
		t.Fatal("parseHeader accepted a header without the required columns")
	}
	for _, leak := range []string{"secret", "hunter2", "token"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q echoes the header content %q", err, leak)
		}
	}
}

func TestParseHeader_DuplicateNameFirstOccurrenceWins(t *testing.T) {
	cols := append(append([]string{}, recRequired...), "code", "brands")
	h := recMustHeader(t, recJoin(cols...))
	if h.width != len(cols) {
		t.Errorf("width = %d, want %d", h.width, len(cols))
	}
	line := recJoin("111", "Name", "First", "en:c1", "en:us", "b", "222", "Last")

	got, reason := h.decode([]byte(line))

	if reason != skipNone {
		t.Fatalf("decode reason = %v, want none", reason)
	}
	if got.code != "111" || !reflect.DeepEqual(got.brands, []string{"First"}) {
		t.Errorf("decode code = %q, brands = %q; want the first occurrences 111 and [First]", got.code, got.brands)
	}
}

func TestParseHeader_DoesNotKeepTheLine(t *testing.T) {
	line := []byte(recJoin(recRequired...))
	h := recMustHeader(t, string(line))
	h2, err := parseHeader(line)
	if err != nil {
		t.Fatal(err)
	}
	for i := range line {
		line[i] = 'z' // the reader reuses its buffer
	}

	got, reason := h2.decode([]byte(recLineFor(recRequired)))

	if reason != skipNone || !reflect.DeepEqual(got, recWant) {
		t.Errorf("decode after the header buffer was overwritten = %+v, %v", got, reason)
	}
	if h != h2 {
		t.Errorf("headers differ: %+v vs %+v", h, h2)
	}
}
