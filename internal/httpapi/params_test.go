package httpapi

import (
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	msgMalformed = "malformed query string"
	msgUnknown   = "unknown parameter; allowed parameters are q, page and size"
	msgUTF8      = "q must be valid UTF-8"
	msgControl   = "q must not contain control characters"
	msgQLen      = "q must be at most 100 characters"
	msgPage      = "page must be an integer >= 1"
	msgSize      = "size must be an integer between 1 and 100"
)

func TestParseSearchParams(t *testing.T) {
	invalid := func(msg string) *apiError {
		return &apiError{status: 400, code: "invalid_parameter", message: msg}
	}
	outOfRange := func(msg string) *apiError {
		return &apiError{status: 400, code: "page_out_of_range", message: msg}
	}
	q := func(text string, page, size int) catalog.Query {
		return catalog.Query{Text: text, Page: page, Size: size}
	}
	e100 := strings.Repeat("é", 100) // 100 runes, each 2 bytes
	pe := strings.Repeat("%C3%A9", 1)

	tests := []struct {
		name    string
		raw     string
		want    catalog.Query
		wantErr *apiError
	}{
		// defaults
		{name: "empty query string", raw: "", want: q("", 1, 20)},
		{name: "q empty value is absent", raw: "q=", want: q("", 1, 20)},
		{name: "q without equals is absent", raw: "q", want: q("", 1, 20)},
		{name: "empty page and size use defaults", raw: "page=&size=", want: q("", 1, 20)},
		{name: "q trimmed", raw: "q=+choc+", want: q("choc", 1, 20)},
		{name: "all parameters", raw: "q=tea&page=3&size=50", want: q("tea", 3, 50)},

		// q length
		{name: "100 runes accepted", raw: "q=" + strings.Repeat(pe, 100), want: q(e100, 1, 20)},
		{name: "101 runes refused", raw: "q=" + strings.Repeat(pe, 101), wantErr: invalid(msgQLen)},
		{name: "102 raw runes trimmed to 100 accepted", raw: "q=+" + strings.Repeat(pe, 100) + "+", want: q(e100, 1, 20)},
		{name: "101 runes plus spaces refused", raw: "q=+" + strings.Repeat(pe, 101) + "+", wantErr: invalid(msgQLen)},

		// q content
		{name: "invalid utf-8", raw: "q=%FF", wantErr: invalid(msgUTF8)},
		{name: "nul", raw: "q=%00", wantErr: invalid(msgControl)},
		{name: "tab inside", raw: "q=a%09b", wantErr: invalid(msgControl)},
		{name: "C1 control inside", raw: "q=a%C2%85b", wantErr: invalid(msgControl)},
		{name: "bare C1 NEL is trimmed away", raw: "q=%C2%85", want: q("", 1, 20)},
		{name: "line separator inside is not control", raw: "q=a%E2%80%A8b", want: q("a b", 1, 20)},
		{name: "bare line separator accepted", raw: "q=%E2%80%A8", want: q("", 1, 20)},
		{name: "control after trim at edge", raw: "q=%01a", wantErr: invalid(msgControl)},
		{name: "utf-8 checked before trim and length", raw: "q=%FF" + strings.Repeat("a", 200), wantErr: invalid(msgUTF8)},
		{name: "control checked before length", raw: "q=%00" + strings.Repeat("a", 200), wantErr: invalid(msgControl)},

		// page
		{name: "page 0", raw: "page=0", wantErr: invalid(msgPage)},
		{name: "page negative", raw: "page=-1", wantErr: invalid(msgPage)},
		{name: "page plus sign", raw: "page=%2B2", wantErr: invalid(msgPage)},
		{name: "page literal plus is a space", raw: "page=+2", wantErr: invalid(msgPage)},
		{name: "page leading space", raw: "page=%202", wantErr: invalid(msgPage)},
		{name: "page decimal", raw: "page=1.5", wantErr: invalid(msgPage)},
		{name: "page letters", raw: "page=abc", wantErr: invalid(msgPage)},
		{name: "page overflow", raw: "page=99999999999999999999", wantErr: invalid(msgPage)},
		{name: "page leading zeros", raw: "page=007", want: q("", 7, 20)},
		{name: "page leading zero is decimal not octal", raw: "page=010", want: q("", 10, 20)},
		{name: "page 00 is zero", raw: "page=00", wantErr: invalid(msgPage)},
		{name: "page slash char below digits", raw: "page=%2F", wantErr: invalid(msgPage)},
		{name: "page colon char above digits", raw: "page=%3A", wantErr: invalid(msgPage)},
		{name: "page digit 9 accepted", raw: "page=9&size=1", want: q("", 9, 1)},
		{name: "page digit 0 inside", raw: "page=10&size=1", want: q("", 10, 1)},

		// size
		{name: "size 0", raw: "size=0", wantErr: invalid(msgSize)},
		{name: "size 101", raw: "size=101", wantErr: invalid(msgSize)},
		{name: "size negative", raw: "size=-5", wantErr: invalid(msgSize)},
		{name: "size letter", raw: "size=x", wantErr: invalid(msgSize)},
		{name: "size plus", raw: "size=%2B5", wantErr: invalid(msgSize)},
		{name: "size overflow", raw: "size=99999999999999999999", wantErr: invalid(msgSize)},
		{name: "size 1", raw: "size=1", want: q("", 1, 1)},
		{name: "size 100", raw: "size=100", want: q("", 1, 100)},
		{name: "size slash char", raw: "size=%2F", wantErr: invalid(msgSize)},
		{name: "size colon char", raw: "size=%3A", wantErr: invalid(msgSize)},
		{name: "size 9", raw: "size=9", want: q("", 1, 9)},
		{name: "size 05", raw: "size=05", want: q("", 1, 5)},

		// window
		{name: "page 500 size 20", raw: "page=500&size=20", want: q("", 500, 20)},
		{name: "page 501 size 20", raw: "page=501&size=20", wantErr: outOfRange("page must be at most 500 when size is 20")},
		{name: "page 334 size 30", raw: "page=334&size=30", wantErr: outOfRange("page must be at most 333 when size is 30")},
		{name: "page 333 size 30", raw: "page=333&size=30", want: q("", 333, 30)},
		{name: "page 10000 size 1", raw: "page=10000&size=1", want: q("", 10000, 1)},
		{name: "page 10001 size 1", raw: "page=10001&size=1", wantErr: outOfRange("page must be at most 10000 when size is 1")},
		{name: "page 100 size 100", raw: "page=100&size=100", want: q("", 100, 100)},
		{name: "page 101 size 100", raw: "page=101&size=100", wantErr: outOfRange("page must be at most 100 when size is 100")},
		{name: "page max int64 size 100", raw: "page=9223372036854775807&size=100", wantErr: outOfRange("page must be at most 100 when size is 100")},
		{name: "default size window", raw: "page=501", wantErr: outOfRange("page must be at most 500 when size is 20")},

		// keys
		{name: "unknown key", raw: "foo=1", wantErr: invalid(msgUnknown)},
		{name: "keys are case sensitive", raw: "Q=x", wantErr: invalid(msgUnknown)},
		{name: "q twice", raw: "q=a&q=b", wantErr: invalid("parameter q must appear at most once")},
		{name: "q twice empty", raw: "q=&q=", wantErr: invalid("parameter q must appear at most once")},
		{name: "page twice", raw: "page=1&page=1", wantErr: invalid("parameter page must appear at most once")},
		{name: "size twice", raw: "size=1&size=1", wantErr: invalid("parameter size must appear at most once")},

		// malformed
		{name: "bad escape", raw: "q=%zz", wantErr: invalid(msgMalformed)},
		{name: "semicolon", raw: "q=a;b", wantErr: invalid(msgMalformed)},

		// order
		{name: "malformed beats unknown", raw: "foo=1&q=%zz", wantErr: invalid(msgMalformed)},
		{name: "unknown beats size", raw: "foo=1&size=0", wantErr: invalid(msgUnknown)},
		{name: "q error beats size", raw: "q=%FF&size=0", wantErr: invalid(msgUTF8)},
		{name: "q error beats page", raw: "q=%FF&page=0", wantErr: invalid(msgUTF8)},
		{name: "page error beats size", raw: "size=0&page=0", wantErr: invalid(msgPage)},
		{name: "size error beats window", raw: "page=99999&size=101", wantErr: invalid(msgSize)},
		{name: "sorted keys unknown first", raw: "aaa=1&q=a&q=b", wantErr: invalid(msgUnknown)},
		{name: "sorted keys repeat first", raw: "q=a&q=b&zzz=1", wantErr: invalid("parameter q must appear at most once")},
		{name: "sorted keys page before size", raw: "size=1&size=1&page=1&page=1", wantErr: invalid("parameter page must appear at most once")},
		{name: "repeat beats q content", raw: "q=%FF&q=%FF", wantErr: invalid("parameter q must appear at most once")},
		{name: "repeat beats value errors", raw: "page=0&size=1&size=2", wantErr: invalid("parameter size must appear at most once")},
		{name: "q control beats q length", raw: "q=" + strings.Repeat("a", 101) + "%01", wantErr: invalid(msgControl)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 20; i++ { // map iteration order must not matter
				got, err := parseSearchParams(tt.raw)
				if (err == nil) != (tt.wantErr == nil) {
					t.Fatalf("parseSearchParams(%q) err = %v, want %v", tt.raw, err, tt.wantErr)
				}
				if tt.wantErr != nil {
					if *err != *tt.wantErr {
						t.Fatalf("parseSearchParams(%q) err = %+v, want %+v", tt.raw, *err, *tt.wantErr)
					}
					continue
				}
				if got != tt.want {
					t.Fatalf("parseSearchParams(%q) = %+v, want %+v", tt.raw, got, tt.want)
				}
			}
		})
	}
}

func TestParseSearchParams_ErrorsNeverEchoInput(t *testing.T) {
	const marker = "zq9marker"
	for _, raw := range []string{
		"q=%FF" + marker, "q=" + marker + "%00", "page=" + marker, "size=" + marker,
		marker + "=1", marker + "=1&" + marker + "=2", "q=" + marker + "%zz", "q=" + marker + ";b",
		"q=" + marker + "&q=" + marker, "page=" + marker + "&size=1",
	} {
		_, err := parseSearchParams(raw)
		if err == nil {
			t.Fatalf("parseSearchParams(%q) accepted", raw)
		}
		if strings.Contains(err.message, marker) || strings.Contains(err.code, marker) {
			t.Errorf("parseSearchParams(%q) echoes input: %+v", raw, *err)
		}
	}
}
