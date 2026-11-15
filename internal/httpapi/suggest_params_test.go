package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	unknownSuggestMsg = "unknown parameter; allowed parameters are q and size"
	suggestSizeMsg    = "size must be an integer between 1 and 20"
	qUTF8Msg          = "q must be valid UTF-8"
	qControlMsg       = "q must not contain control characters"
	qLongMsg          = "q must be at most 100 characters"
	qOnceMsg          = "parameter q must appear at most once"
	sizeOnceMsg       = "parameter size must appear at most once"
	malformedMsg      = "malformed query string"
)

func TestParseSuggestParams(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    catalog.SuggestQuery
		wantMsg string
	}{
		{name: "absent query", raw: "", want: catalog.SuggestQuery{Text: "", Size: 10}},
		{name: "empty q", raw: "q=", want: catalog.SuggestQuery{Text: "", Size: 10}},
		{name: "spaces only q", raw: "q=%20%20", want: catalog.SuggestQuery{Text: "", Size: 10}},
		{name: "plus spaces trimmed", raw: "q=+choc+", want: catalog.SuggestQuery{Text: "choc", Size: 10}},
		{name: "case preserved", raw: "q=%20Choc%20%C3%89", want: catalog.SuggestQuery{Text: "Choc É", Size: 10}},
		{name: "one rune is not an error", raw: "q=a", want: catalog.SuggestQuery{Text: "a", Size: 10}},
		{name: "100 runes accepted", raw: "q=" + strings.Repeat("%C3%A9", 100), want: catalog.SuggestQuery{Text: strings.Repeat("é", 100), Size: 10}},
		{name: "101 runes refused", raw: "q=" + strings.Repeat("%C3%A9", 101), wantMsg: qLongMsg},
		{name: "invalid utf8", raw: "q=%FF", wantMsg: qUTF8Msg},
		{name: "nul", raw: "q=%00", wantMsg: qControlMsg},
		{name: "tab inside", raw: "q=a%09b", wantMsg: qControlMsg},
		{name: "repeated q", raw: "q=a&q=b", wantMsg: qOnceMsg},
		{name: "bad escape", raw: "q=%zz", wantMsg: malformedMsg},
		{name: "semicolon", raw: "q=a;b", wantMsg: malformedMsg},

		{name: "size 1", raw: "q=ab&size=1", want: catalog.SuggestQuery{Text: "ab", Size: 1}},
		{name: "size 20", raw: "q=ab&size=20", want: catalog.SuggestQuery{Text: "ab", Size: 20}},
		{name: "size 0", raw: "size=0", wantMsg: suggestSizeMsg},
		{name: "size 21", raw: "size=21", wantMsg: suggestSizeMsg},
		{name: "size negative", raw: "size=-1", wantMsg: suggestSizeMsg},
		{name: "size plus", raw: "size=%2B2", wantMsg: suggestSizeMsg},
		{name: "size plus literal is space", raw: "size=+2", wantMsg: suggestSizeMsg},
		{name: "size letters", raw: "size=abc", wantMsg: suggestSizeMsg},
		{name: "size decimal", raw: "size=1.5", wantMsg: suggestSizeMsg},
		{name: "size leading space", raw: "size=%202", wantMsg: suggestSizeMsg},
		{name: "size overflow", raw: "size=99999999999999999999", wantMsg: suggestSizeMsg},
		{name: "size empty", raw: "size=", want: catalog.SuggestQuery{Text: "", Size: 10}},
		{name: "size repeated", raw: "size=5&size=6", wantMsg: sizeOnceMsg},
		{name: "size leading zero is decimal", raw: "size=010", want: catalog.SuggestQuery{Text: "", Size: 10}},
		{name: "size fullwidth digits", raw: "size=%EF%BC%91%EF%BC%92", wantMsg: suggestSizeMsg},

		{name: "unknown page", raw: "page=1", wantMsg: unknownSuggestMsg},
		{name: "unknown brands", raw: "brands=x", wantMsg: unknownSuggestMsg},
		{name: "unknown Q", raw: "Q=x", wantMsg: unknownSuggestMsg},
		{name: "unknown foo", raw: "foo=1", wantMsg: unknownSuggestMsg},
		{name: "unknown html key", raw: "%3Cb%3E=1", wantMsg: unknownSuggestMsg},
		{name: "unknown 33 rune key", raw: strings.Repeat("k", 33) + "=1", wantMsg: unknownSuggestMsg},
		{name: "unknown nul key", raw: "%00=1", wantMsg: unknownSuggestMsg},
		{name: "unknown empty key", raw: "=1", wantMsg: unknownSuggestMsg},
		{name: "unknown key with space", raw: "brand%20x=1", wantMsg: unknownSuggestMsg},

		{name: "sorted: unknown before q error", raw: "aaa=1&q=%FF", wantMsg: unknownSuggestMsg},
		{name: "sorted: repeated q before size error", raw: "q=a&q=b&size=0", wantMsg: qOnceMsg},
		{name: "sorted: q error before size error", raw: "q=%FF&size=0", wantMsg: qUTF8Msg},
		{name: "sorted: unknown after size error", raw: "size=0&zzz=1", wantMsg: unknownSuggestMsg},
		{name: "malformed before unknown", raw: "foo=1&q=%zz", wantMsg: malformedMsg},
		{name: "repeated q before repeated size", raw: "size=5&size=6&q=a&q=b", wantMsg: qOnceMsg},
		{name: "unknown before repeated q", raw: "q=a&q=b&foo=1", wantMsg: unknownSuggestMsg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, aerr := parseSuggestParams(tt.raw)
			if tt.wantMsg != "" {
				if aerr == nil {
					t.Fatalf("parseSuggestParams(%q) = %+v, want error %q", tt.raw, got, tt.wantMsg)
				}
				if aerr.status != http.StatusBadRequest || aerr.code != codeInvalidParameter || aerr.message != tt.wantMsg {
					t.Fatalf("parseSuggestParams(%q) error = %d %s %q, want 400 %s %q",
						tt.raw, aerr.status, aerr.code, aerr.message, codeInvalidParameter, tt.wantMsg)
				}
				return
			}
			if aerr != nil {
				t.Fatalf("parseSuggestParams(%q) error = %q, want none", tt.raw, aerr.message)
			}
			if got != tt.want {
				t.Fatalf("parseSuggestParams(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSuggestConstants(t *testing.T) {
	if defaultSuggestSize != 10 || maxSuggestSize != 20 || minSuggestRunes != 2 {
		t.Fatalf("constants = %d, %d, %d; want 10, 20, 2", defaultSuggestSize, maxSuggestSize, minSuggestRunes)
	}
}

// The marker must appear in no message, whichever key or value carries it.
func TestParseSuggestParams_NeverEchoes(t *testing.T) {
	const m = "ZQXMARKER"
	raws := []string{
		m + "=1",
		"q=" + m + "&" + m + "=1",
		"size=" + m,
		"size=" + m + "&size=" + m,
		"q=" + m + "&q=" + m,
		"q=" + m + "%FF",
		"q=" + m + "%00",
		"q=" + m + "%zz",
		"q=" + m + ";x",
		"q=" + strings.Repeat(m, 20),
		"size=" + m + "%FF",
		"%FF" + m + "=1",
		m + "%00=1",
		"Q=" + m,
	}
	for _, raw := range raws {
		_, aerr := parseSuggestParams(raw)
		if aerr == nil {
			t.Errorf("parseSuggestParams(%q): want an error", raw)
			continue
		}
		if strings.Contains(aerr.message, m) {
			t.Errorf("parseSuggestParams(%q) message %q echoes the input", raw, aerr.message)
		}
	}
}
