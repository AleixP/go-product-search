package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Process exit codes.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitInterrupted = 130
)

const (
	defaultInput       = "https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz"
	defaultOpenSearch  = "http://localhost:9200"
	defaultCountries   = "en:united-states"
	defaultRequire     = "brand,category,nutriscore"
	defaultLimit       = 100000
	defaultSampleEvery = 2
)

// allowedRequire lists the values --require accepts, in the order shown in errors.
var allowedRequire = []string{"brand", "category", "nutriscore"}

var countryTag = regexp.MustCompile(`^[a-z]{2}:[a-z0-9-]+$`)

// config is the validated command line of the ingest command.
type config struct {
	Input       string   // local path or https URL of the CSV
	OpenSearch  string   // http or https address of the cluster
	Countries   []string // taxonomy tags, empty means no country filter
	Require     []string // required fields, subset of allowedRequire
	Limit       int      // maximum indexed products, 0 means no limit
	SampleEvery int      // keep one product in every SampleEvery
	DryRun      bool
}

// newFlagSet builds the flag set; its output goes to out.
func newFlagSet(out io.Writer, raw *rawFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&raw.input, "input", defaultInput, "local path or https URL of the Open Food Facts CSV (gzip detected automatically)")
	fs.StringVar(&raw.opensearch, "opensearch", defaultOpenSearch, "OpenSearch address (http or https)")
	fs.StringVar(&raw.countries, "countries", defaultCountries, "comma-separated country taxonomy tags to keep; empty keeps all countries")
	fs.StringVar(&raw.require, "require", defaultRequire, "comma-separated fields a product must have: brand, category, nutriscore; empty requires none")
	fs.IntVar(&raw.limit, "limit", defaultLimit, "maximum number of products to index; 0 means no limit")
	fs.IntVar(&raw.sampleEvery, "sample-every", defaultSampleEvery, "keep one eligible product in every N (N >= 1)")
	fs.BoolVar(&raw.dryRun, "dry-run", false, "read and count the input without touching OpenSearch")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(out, "Usage: ingest [flags]")
		fs.PrintDefaults()
	}
	return fs
}

// rawFlags holds the flag values as typed, before validation.
type rawFlags struct {
	input, opensearch, countries, require string
	limit, sampleEvery                    int
	dryRun                                bool
}

// parseConfig parses and validates the command line. It returns flag.ErrHelp
// for -h/--help. Error messages never contain userinfo, path or query of a URL.
func parseConfig(args []string) (config, error) {
	var raw rawFlags
	fs := newFlagSet(io.Discard, &raw)
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument(s): %d positional argument(s) given, none allowed", fs.NArg())
	}
	if raw.limit < 0 {
		return config{}, errors.New("--limit must be >= 0 (0 means no limit)")
	}
	if raw.sampleEvery < 1 {
		return config{}, errors.New("--sample-every must be >= 1")
	}
	if err := validateInput(raw.input); err != nil {
		return config{}, err
	}
	if err := validateOpenSearch(raw.opensearch); err != nil {
		return config{}, err
	}
	countries, err := parseCountries(raw.countries)
	if err != nil {
		return config{}, err
	}
	require, err := parseRequire(raw.require)
	if err != nil {
		return config{}, err
	}
	return config{
		Input:       raw.input,
		OpenSearch:  raw.opensearch,
		Countries:   countries,
		Require:     require,
		Limit:       raw.limit,
		SampleEvery: raw.sampleEvery,
		DryRun:      raw.dryRun,
	}, nil
}

// validateInput accepts a local path or an https URL without credentials.
func validateInput(s string) error {
	if s == "" {
		return errors.New("--input must not be empty")
	}
	if !strings.Contains(s, "://") {
		return nil // local path
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("--input is not a valid URL")
	}
	// url.Parse lowercases the scheme, so HTTPS:// is accepted and HTTP:// is not.
	if u.Scheme != "https" {
		return fmt.Errorf("--input URL must use https, got %s", redact(s))
	}
	if u.User != nil {
		return fmt.Errorf("--input URL must not contain credentials (%s)", redact(s))
	}
	// Hostname, not Host: "https://:443/x" has Host ":443" but no host name.
	// Keep identical to classifyInput in source.go.
	if u.Hostname() == "" {
		return errors.New("--input URL has no host")
	}
	return nil
}

// validateOpenSearch accepts an http or https URL with a host and no credentials.
func validateOpenSearch(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("--opensearch must be an http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("--opensearch must be an http or https URL, got %s", redact(s))
	}
	if u.User != nil {
		return fmt.Errorf("--opensearch must not contain credentials (%s)", redact(s))
	}
	// Hostname, not Host: "http://:9200" has Host ":9200" but no host name.
	if u.Hostname() == "" {
		return errors.New("--opensearch URL has no host")
	}
	return nil
}

// redact keeps only the scheme and host of a URL for error messages.
func redact(addr string) string {
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "(unparsable)"
}

// splitList splits a comma-separated list, trims each item, rejects empty items
// and collapses duplicates keeping the first occurrence.
func splitList(flagName, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("--%s has an empty item", flagName)
		}
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out, nil
}

// parseCountries splits --countries and checks each item is a taxonomy tag
// such as en:united-states. An empty value returns no tags (filter off).
func parseCountries(value string) ([]string, error) {
	tags, err := splitList("countries", value)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		if !countryTag.MatchString(tag) {
			return nil, fmt.Errorf("--countries: %q is not a taxonomy tag like en:united-states", tag)
		}
	}
	return tags, nil
}

// parseRequire splits --require and checks each item is in allowedRequire.
// An empty value returns no names (nothing required).
func parseRequire(value string) ([]string, error) {
	names, err := splitList("require", value)
	if err != nil {
		return nil, fmt.Errorf("%w; allowed values: %s", err, strings.Join(allowedRequire, ", "))
	}
	for _, name := range names {
		if !isAllowedRequire(name) {
			return nil, fmt.Errorf("--require: unknown value %q; allowed values: %s", name, strings.Join(allowedRequire, ", "))
		}
	}
	return names, nil
}

// isAllowedRequire reports whether name is one of allowedRequire.
func isAllowedRequire(name string) bool {
	for _, a := range allowedRequire {
		if name == a {
			return true
		}
	}
	return false
}
