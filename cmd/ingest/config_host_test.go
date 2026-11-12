package main

import (
	"strings"
	"testing"
)

// A URL with a port but no host name has a non-empty Host (":443"): it must be
// rejected as having no host, and the value must never be echoed.
func TestParseConfig_InputWithoutHostNameButWithPortIsRejected(t *testing.T) {
	for _, in := range []string{"https://:443/a.csv?k=s3cr3t", "https://:8080", "https://:9/s3cr3t"} {
		_, err := parseConfig([]string{"--input", in})
		if err == nil {
			t.Errorf("parseConfig(--input %q) accepted an input without a host name", in)
			continue
		}
		if !strings.Contains(err.Error(), "host") {
			t.Errorf("parseConfig(--input %q) error = %q, want it to mention the host", in, err)
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "443") || strings.Contains(err.Error(), "8080") {
			t.Errorf("parseConfig(--input %q) error echoes the input: %q", in, err)
		}
	}
}

// --opensearch with a port but no host name has a non-empty Host: it is as
// unusable as an empty one.
func TestParseConfig_OpenSearchWithoutHostNameButWithPortIsRejected(t *testing.T) {
	for _, in := range []string{"https://:9200", "http://:9200", "http://:9200/s3cr3t"} {
		_, err := parseConfig([]string{"--opensearch", in})
		if err == nil {
			t.Errorf("parseConfig(--opensearch %q) accepted a URL without a host name", in)
			continue
		}
		if !strings.Contains(err.Error(), "host") || strings.Contains(err.Error(), "9200") {
			t.Errorf("parseConfig(--opensearch %q) error = %q, want it to mention the host and not echo the input", in, err)
		}
	}
}
