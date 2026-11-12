package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestExitCodes(t *testing.T) {
	if exitOK != 0 || exitFailure != 1 || exitUsage != 2 || exitInterrupted != 130 {
		t.Fatalf("exit codes = %d %d %d %d, want 0 1 2 130", exitOK, exitFailure, exitUsage, exitInterrupted)
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	got, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil) error = %v", err)
	}
	want := config{
		Input:       "https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz",
		OpenSearch:  "http://localhost:9200",
		Countries:   []string{"en:united-states"},
		Require:     []string{"brand", "category", "nutriscore"},
		Limit:       100000,
		SampleEvery: 2,
		DryRun:      false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseConfig(nil) = %+v, want %+v", got, want)
	}
}

func TestParseConfig_EveryFlagSet(t *testing.T) {
	got, err := parseConfig([]string{
		"--input", "data/x.csv.gz",
		"--opensearch", "https://search.example.com:9243",
		"--countries", "en:france,en:canada",
		"--require", "brand",
		"--limit", "50",
		"--sample-every", "3",
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("parseConfig error = %v", err)
	}
	want := config{
		Input:       "data/x.csv.gz",
		OpenSearch:  "https://search.example.com:9243",
		Countries:   []string{"en:france", "en:canada"},
		Require:     []string{"brand"},
		Limit:       50,
		SampleEvery: 3,
		DryRun:      true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseConfig = %+v, want %+v", got, want)
	}
}

func TestParseConfig_SingleDashAndEquals(t *testing.T) {
	got, err := parseConfig([]string{"-limit=7", "-dry-run"})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if got.Limit != 7 || !got.DryRun {
		t.Errorf("got Limit=%d DryRun=%v, want 7 true", got.Limit, got.DryRun)
	}
}

func TestParseConfig_Require(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty gives none", "", nil},
		{"only spaces gives none", "   ", nil},
		{"trimmed", " brand , nutriscore", []string{"brand", "nutriscore"}},
		{"order kept", "nutriscore,brand", []string{"nutriscore", "brand"}},
		{"duplicates collapsed", "brand,category,brand,category", []string{"brand", "category"}},
		{"all", "brand,category,nutriscore", []string{"brand", "category", "nutriscore"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfig([]string{"--require", tt.in})
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if len(got.Require) != len(tt.want) || (len(tt.want) > 0 && !reflect.DeepEqual(got.Require, tt.want)) {
				t.Errorf("Require = %#v, want %#v", got.Require, tt.want)
			}
		})
	}
}

func TestParseConfig_Countries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty turns the filter off", "", nil},
		{"spaces only turns the filter off", "  ", nil},
		{"one", "en:united-states", []string{"en:united-states"}},
		{"trimmed", " en:france , en:canada ", []string{"en:france", "en:canada"}},
		{"duplicates collapsed", "en:france,en:canada,en:france", []string{"en:france", "en:canada"}},
		{"two letter country", "en:us", []string{"en:us"}},
		{"digits and dashes", "fr:royaume-uni-2", []string{"fr:royaume-uni-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfig([]string{"--countries", tt.in})
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if len(got.Countries) != len(tt.want) || (len(tt.want) > 0 && !reflect.DeepEqual(got.Countries, tt.want)) {
				t.Errorf("Countries = %#v, want %#v", got.Countries, tt.want)
			}
		})
	}
}

func TestParseConfig_AcceptedValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ok   func(config) bool
	}{
		{"relative dot path", []string{"--input", "./data/x.csv.gz"}, func(c config) bool { return c.Input == "./data/x.csv.gz" }},
		{"path with colon", []string{"--input", "data/a:b.csv.gz"}, func(c config) bool { return c.Input == "data/a:b.csv.gz" }},
		{"relative path", []string{"--input", "data/x.csv.gz"}, func(c config) bool { return c.Input == "data/x.csv.gz" }},
		{"absolute path", []string{"--input", "/var/data/x.csv.gz"}, func(c config) bool { return c.Input == "/var/data/x.csv.gz" }},
		{"https url", []string{"--input", "https://example.com/a.csv.gz?x=1"}, func(c config) bool { return c.Input == "https://example.com/a.csv.gz?x=1" }},
		{"https upper case scheme", []string{"--input", "HTTPS://example.com/a"}, func(c config) bool { return c.Input == "HTTPS://example.com/a" }},
		{"https mixed case scheme", []string{"--input", "HtTpS://example.com/a"}, func(c config) bool { return c.Input == "HtTpS://example.com/a" }},
		{"opensearch https", []string{"--opensearch", "https://os.example.com:9200"}, func(c config) bool { return c.OpenSearch == "https://os.example.com:9200" }},
		{"opensearch upper case scheme", []string{"--opensearch", "HTTP://localhost:9200"}, func(c config) bool { return c.OpenSearch == "HTTP://localhost:9200" }},
		{"limit zero", []string{"--limit", "0"}, func(c config) bool { return c.Limit == 0 }},
		{"limit max int", []string{"--limit", strconv.Itoa(math.MaxInt)}, func(c config) bool { return c.Limit == math.MaxInt }},
		{"sample-every one", []string{"--sample-every", "1"}, func(c config) bool { return c.SampleEvery == 1 }},
		{"sample-every max int", []string{"--sample-every", strconv.Itoa(math.MaxInt)}, func(c config) bool { return c.SampleEvery == math.MaxInt }},
		{"dry-run explicit false", []string{"--dry-run=false"}, func(c config) bool { return !c.DryRun }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfig(tt.args)
			if err != nil {
				t.Fatalf("parseConfig(%q) error = %v", tt.args, err)
			}
			if !tt.ok(got) {
				t.Errorf("parseConfig(%q) = %+v", tt.args, got)
			}
		})
	}
}

var rejected = []struct {
	name string
	args []string
	want string // substring the error must contain
}{
	{"input http", []string{"--input", "http://x"}, "https"},
	{"input http upper case", []string{"--input", "HTTP://x/a"}, "https"},
	{"input file scheme", []string{"--input", "file:///etc/passwd"}, "https"},
	{"input ftp", []string{"--input", "ftp://x"}, "https"},
	{"input https prefix only", []string{"--input", "httpsx://x/a"}, "https"},
	{"input with userinfo", []string{"--input", "https://u:secret@h/x"}, "credentials"},
	{"input with user only", []string{"--input", "https://secret@h/x"}, "credentials"},
	{"input with empty username and password", []string{"--input", "https://:secret@h/x"}, "credentials"},
	{"input with empty userinfo", []string{"--input", "https://@h/x"}, "credentials"},
	{"input without host", []string{"--input", "https://"}, "host"},
	{"input without host with path", []string{"--input", "https:///a.csv"}, "host"},
	{"input empty", []string{"--input", ""}, "input"},
	{"input bad url", []string{"--input", "https://h:bad/x"}, "input"},
	{"opensearch scheme prefix of http", []string{"--opensearch", "httpx://h:9200"}, "opensearch"},
	{"opensearch ftp", []string{"--opensearch", "ftp://x"}, "opensearch"},
	{"opensearch with userinfo", []string{"--opensearch", "http://u:secret@h:9200"}, "credentials"},
	{"opensearch without host", []string{"--opensearch", "http://"}, "host"},
	{"opensearch empty", []string{"--opensearch", ""}, "opensearch"},
	{"opensearch not a url", []string{"--opensearch", "localhost:9200"}, "opensearch"},
	{"limit negative", []string{"--limit", "-1"}, "limit"},
	{"limit not a number", []string{"--limit", "abc"}, "limit"},
	{"limit overflow", []string{"--limit", "99999999999999999999"}, "limit"},
	{"sample-every zero", []string{"--sample-every", "0"}, "sample-every"},
	{"sample-every negative", []string{"--sample-every", "-1"}, "sample-every"},
	{"sample-every not a number", []string{"--sample-every", "x"}, "sample-every"},
	{"require unknown", []string{"--require", "foo"}, "brand, category, nutriscore"},
	{"require unknown among valid", []string{"--require", "brand,foo"}, "brand, category, nutriscore"},
	{"require wrong case", []string{"--require", "Brand"}, "brand, category, nutriscore"},
	{"require empty item", []string{"--require", "brand,,category"}, "brand, category, nutriscore"},
	{"require trailing comma", []string{"--require", "brand,"}, "brand, category, nutriscore"},
	{"countries upper case", []string{"--countries", "United-States"}, "countries"},
	{"countries upper case after prefix", []string{"--countries", "en:US"}, "countries"},
	{"countries upper case prefix", []string{"--countries", "EN:us"}, "countries"},
	{"countries empty item", []string{"--countries", "en:united-states,,en:canada"}, "countries"},
	{"countries trailing comma", []string{"--countries", "en:canada,"}, "countries"},
	{"countries empty name", []string{"--countries", "en:"}, "countries"},
	{"countries empty prefix", []string{"--countries", ":us"}, "countries"},
	{"countries one letter prefix", []string{"--countries", "e:us"}, "countries"},
	{"countries three letter prefix", []string{"--countries", "eng:us"}, "countries"},
	{"countries underscore", []string{"--countries", "en:u_s"}, "countries"},
	{"countries two colons", []string{"--countries", "en:us:x"}, "countries"},
	{"countries inner space", []string{"--countries", "en:united states"}, "countries"},
	{"countries trailing text after newline", []string{"--countries", "en:us\nx"}, "countries"},
	{"countries leading text", []string{"--countries", "xen:us,ab"}, "countries"},
	{"countries digit prefix", []string{"--countries", "e1:us"}, "countries"},
	{"unknown flag", []string{"--bogus"}, "bogus"},
	{"positional argument", []string{"extra"}, "argument"},
	{"positional after flags", []string{"--dry-run", "extra"}, "argument"},
}

func TestParseConfig_Rejected(t *testing.T) {
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfig(tt.args)
			if err == nil {
				t.Fatalf("parseConfig(%q) error = nil, want an error", tt.args)
			}
			if errors.Is(err, flag.ErrHelp) {
				t.Fatalf("parseConfig(%q) returned ErrHelp", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseConfig(%q) error = %q, want it to contain %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestRun_RejectedExitsWithUsage(t *testing.T) {
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := runArgs(tt.args, &stderr); code != exitUsage {
				t.Fatalf("run(%q) = %d, want %d", tt.args, code, exitUsage)
			}
			out := stderr.String()
			errorLines := 0
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "ingest: ") {
					errorLines++
				}
			}
			if errorLines != 1 {
				t.Errorf("stderr has %d lines starting with %q, want exactly 1:\n%s", errorLines, "ingest: ", out)
			}
			first, _, _ := strings.Cut(out, "\n")
			if !strings.HasPrefix(first, "ingest: ") || !strings.Contains(first, tt.want) {
				t.Errorf("first line = %q, want a one-line %q error containing %q", first, "ingest: ...", tt.want)
			}
			if _, rest, _ := strings.Cut(out, "\n"); !strings.HasPrefix(rest, "Usage: ingest [flags]\n") {
				t.Errorf("error must be one line followed by the usage, got:\n%s", out)
			}
			if !strings.Contains(out, "Usage: ingest [flags]") || !strings.Contains(out, "-sample-every") {
				t.Errorf("stderr lacks usage:\n%s", out)
			}
		})
	}
}

func TestRun_HelpExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "-help"} {
		t.Run(arg, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := runArgs([]string{arg}, &stderr); code != exitOK {
				t.Fatalf("run(%q) = %d, want %d", arg, code, exitOK)
			}
			out := stderr.String()
			for _, want := range []string{"Usage: ingest [flags]", "-input", "-opensearch", "-countries", "-require", "-limit", "-sample-every", "-dry-run"} {
				if !strings.Contains(out, want) {
					t.Errorf("help lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "ingest: ") {
				t.Errorf("help printed an error line:\n%s", out)
			}
		})
	}
}

func TestUsage_ShowsDefaults(t *testing.T) {
	var stderr bytes.Buffer
	runArgs([]string{"-h"}, &stderr)
	for _, want := range []string{"100000", "en:united-states", "http://localhost:9200", "brand,category,nutriscore", "en.openfoodfacts.org.products.csv.gz"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("usage lacks default %q:\n%s", want, stderr.String())
		}
	}
}

func TestErrors_NeverLeakInputSecrets(t *testing.T) {
	const m = "s3cr3t"
	bad := []string{
		"https://us3r:" + m + "@host/p",
		"https://" + m + "@host/p",
		"ftp://us3r:" + m + "@host/" + m + "?k=" + m,
		"file:///" + m + "/x?k=" + m,
		"https:///" + m + "?k=" + m,
		"https://host:" + m + "/" + m,
		"HTTP://us3r:" + m + "@host/" + m,
		"::" + m + "://host/" + m,
	}
	t.Run("positional argument", func(t *testing.T) {
		args := []string{"https://us3r:" + m + "@host/" + m}
		_, err := parseConfig(args)
		if err == nil {
			t.Fatalf("parseConfig(%q) accepted a positional argument", args)
		}
		var stderr bytes.Buffer
		runArgs(args, &stderr)
		for _, text := range []string{err.Error(), stderr.String()} {
			if strings.Contains(text, m) || strings.Contains(text, "us3r") {
				t.Errorf("output leaks a positional argument: %q", text)
			}
		}
	})
	for _, flagName := range []string{"--input", "--opensearch"} {
		for _, v := range bad {
			t.Run(flagName+" "+v, func(t *testing.T) {
				args := []string{flagName, v}
				_, err := parseConfig(args)
				if err == nil {
					t.Fatalf("parseConfig(%q) accepted a bad value", args)
				}
				var stderr bytes.Buffer
				runArgs(args, &stderr)
				for _, text := range []string{err.Error(), stderr.String()} {
					if strings.Contains(text, m) || strings.Contains(text, "us3r") {
						t.Errorf("output leaks input data: %q", text)
					}
				}
			})
		}
	}
}

func TestErrors_Redaction(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--opensearch", "ftp://host:9200/path?q=1"}, "ftp://host:9200"},
		{[]string{"--opensearch", "https://u:p@host:9200/path?q=1"}, "https://host:9200"},
		{[]string{"--input", "ftp://host/path?q=1"}, "ftp://host"},
		{[]string{"--input", "https://u:p@host/path?q=1"}, "https://host"},
		{[]string{"--input", "ftp://"}, "(unparsable)"},
		{[]string{"--opensearch", "ftp://"}, "(unparsable)"},
		{[]string{"--input", "https://s3cr3t@"}, "(unparsable)"},
	}
	for _, tt := range tests {
		_, err := parseConfig(tt.args)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("parseConfig(%q) error = %v, want it to contain %q", tt.args, err, tt.want)
			continue
		}
		for _, banned := range []string{"/path", "q=1", "u:p"} {
			if strings.Contains(err.Error(), banned) {
				t.Errorf("parseConfig(%q) error %q contains %q", tt.args, err, banned)
			}
		}
	}
}

func TestParseConfig_EmptyItemsAreNamed(t *testing.T) {
	for _, args := range [][]string{{"--countries", "en:a,,en:b"}, {"--require", "brand,,category"}} {
		_, err := parseConfig(args)
		if err == nil || !strings.Contains(err.Error(), "empty item") {
			t.Errorf("parseConfig(%q) error = %v, want it to mention an empty item", args, err)
		}
	}
}

// TestMainHelper runs main() in a child process; it is not a test on its own.
func TestMainHelper(t *testing.T) {
	if os.Getenv("INGEST_TEST_MAIN") != "1" {
		t.Skip("helper for TestMain_ExitCodes")
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{"ingest"}, os.Args[i+1:]...)
			break
		}
	}
	main()
}

// TestMain_ExitCodes checks the real binary wiring: exit code and which stream
// (stdout or stderr) each message goes to.
func TestMain_ExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr func(string) bool
		describe   string
		wantStdout string // substring of stdout; empty means stdout must stay empty
	}{
		{
			"help", []string{"--help"}, exitOK,
			func(s string) bool {
				return strings.HasPrefix(s, "Usage: ingest [flags]\n") && !strings.Contains(s, "ingest: ")
			},
			"the usage and no \"ingest: \" line", "",
		},
		{
			"usage error", []string{"--sample-every", "0"}, exitUsage,
			func(s string) bool {
				return strings.HasPrefix(s, "ingest: --sample-every must be >= 1\nUsage: ingest [flags]\n")
			},
			"the error line followed by the usage", "",
		},
		{
			"valid config",
			[]string{"--dry-run", "--input", "../../testdata/products.tsv", "--sample-every", "1", "--limit", "0"},
			exitOK,
			func(s string) bool { return !strings.Contains(s, "level=ERROR") },
			"logs without an error",
			"rows_read=15\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestMainHelper$", "--"}, tt.args...)...)
			cmd.Env = append(os.Environ(), "INGEST_TEST_MAIN=1")
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			got := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("run %q: %v", tt.args, err)
			}
			if got != tt.wantCode {
				t.Errorf("main with %q exited %d, want %d", tt.args, got, tt.wantCode)
			}
			if tt.wantStdout == "" && stdout.Len() != 0 {
				t.Errorf("main with %q wrote to stdout: %q, want nothing", tt.args, stdout.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("main with %q stdout = %q, want it to contain %q", tt.args, stdout.String(), tt.wantStdout)
			}
			if !tt.wantStderr(stderr.String()) {
				t.Errorf("main with %q stderr = %q, want %s", tt.args, stderr.String(), tt.describe)
			}
		})
	}
}

func TestParseConfig_HelpReturnsErrHelp(t *testing.T) {
	if _, err := parseConfig([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("error = %v, want flag.ErrHelp", err)
	}
}

// runArgs runs the command with the given arguments and no usable input,
// OpenSearch or signals: it is for the usage paths, which end before any of them.
func runArgs(args []string, stderr *bytes.Buffer) int {
	return run(context.Background(), args, io.Discard, stderr, deps{})
}
