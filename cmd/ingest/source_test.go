package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// srcSecret is the marker put in every place a URL secret could hide; no
// error may ever contain it.
const srcSecret = "s3cr3t"

func srcGzip(t testing.TB, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func srcWriteFile(t testing.TB, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// srcIncompressible returns n pseudo-random bytes (deterministic) that gzip
// cannot shrink, so a compressed file is larger than the buffers around it.
func srcIncompressible(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

// srcRepeated is a highly compressible text: decompressed length is far above
// the compressed length.
func srcRepeated(n int) string { return strings.Repeat("line of text\n", n) }

// srcLines reads every line through the Item 5 reader, proving the integration.
func srcLines(t testing.TB, r io.Reader) ([]string, error) {
	t.Helper()
	lr := newLineReader(r, 0)
	var out []string
	for {
		line, tooLong, err := lr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		if tooLong {
			t.Fatal("unexpected too-long line")
		}
		out = append(out, string(line))
	}
}

func srcOpenLocal(t testing.TB, path string) *source {
	t.Helper()
	s, err := openInput(t.Context(), nil, path)
	if err != nil {
		t.Fatalf("openInput(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenInput_LocalPlainFile(t *testing.T) {
	data := "code\tname\n1\tapple\n2\tpear\n"
	s := srcOpenLocal(t, srcWriteFile(t, "p.tsv", []byte(data)))

	lines, err := srcLines(t, s)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{"code\tname", "1\tapple", "2\tpear"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	if s.Size() != int64(len(data)) {
		t.Errorf("Size() = %d, want %d", s.Size(), len(data))
	}
	if s.BytesRead() != int64(len(data)) {
		t.Errorf("BytesRead() = %d, want %d", s.BytesRead(), len(data))
	}
}

func TestOpenInput_LocalGzipFile(t *testing.T) {
	text := srcRepeated(5000)
	z := srcGzip(t, text)
	if len(z) >= len(text)/10 {
		t.Fatalf("fixture is not compressible: %d vs %d", len(z), len(text))
	}
	s := srcOpenLocal(t, srcWriteFile(t, "p.tsv.gz", z))

	got, err := io.ReadAll(s)

	if err != nil {
		t.Fatal(err)
	}
	if string(got) != text {
		t.Errorf("decompressed text differs (len %d, want %d)", len(got), len(text))
	}
	if s.Size() != int64(len(z)) {
		t.Errorf("Size() = %d, want the compressed file size %d", s.Size(), len(z))
	}
	if s.BytesRead() != int64(len(z)) {
		t.Errorf("BytesRead() = %d, want the compressed length %d (not %d decompressed)", s.BytesRead(), len(z), len(got))
	}
}

func TestOpenInput_LocalGzipFileThroughLineReader(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "p.gz", srcGzip(t, "a\nb\nc")))

	lines, err := srcLines(t, s)

	if err != nil || strings.Join(lines, "|") != "a|b|c" {
		t.Errorf("lines = %q, err = %v", lines, err)
	}
}

func TestOpenInput_EmptyFile(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "empty", nil))

	n, err := s.Read(make([]byte, 8))

	if n != 0 || err != io.EOF {
		t.Errorf("Read = %d, %v; want 0, io.EOF", n, err)
	}
	if s.Size() != 0 || s.BytesRead() != 0 {
		t.Errorf("Size/BytesRead = %d/%d, want 0/0", s.Size(), s.BytesRead())
	}
}

// Peek(2) on a one-byte stream returns the byte plus io.EOF: that is plain data.
func TestOpenInput_OneByteFileIsPlain(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "one", []byte("x")))

	got, err := io.ReadAll(s)

	if err != nil || string(got) != "x" {
		t.Errorf("ReadAll = %q, %v; want \"x\", nil", got, err)
	}
}

func TestOpenInput_MagicBytesMustBeExactlyOneFEightB(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"only the first magic byte", []byte{0x1f, 'a', 'b', '\n'}},
		{"only the second magic byte", []byte{'a', 0x8b, 'b', '\n'}},
		{"magic bytes in the wrong order", []byte{0x8b, 0x1f, 'b', '\n'}},
		{"magic bytes not at the start", []byte{'x', 0x1f, 0x8b, '\n'}},
		{"first magic byte then EOF", []byte{0x1f}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := srcOpenLocal(t, srcWriteFile(t, "plain", tt.data))

			got, err := io.ReadAll(s)

			if err != nil || !bytes.Equal(got, tt.data) {
				t.Errorf("ReadAll = %q, %v; want the raw bytes %q, nil", got, err, tt.data)
			}
		})
	}
}

func TestOpenInput_GzipMultistreamReadsAsOneStream(t *testing.T) {
	cat := append(srcGzip(t, "a\nb\n"), srcGzip(t, "c\nd\n")...)
	s := srcOpenLocal(t, srcWriteFile(t, "multi.gz", cat))

	got, err := io.ReadAll(s)

	if err != nil || string(got) != "a\nb\nc\nd\n" {
		t.Errorf("ReadAll = %q, %v; want both members concatenated", got, err)
	}
	if s.BytesRead() != int64(len(cat)) {
		t.Errorf("BytesRead() = %d, want %d", s.BytesRead(), len(cat))
	}
}

func TestOpenInput_LocalGzipFailuresAreNeverCleanEOF(t *testing.T) {
	z := srcGzip(t, srcRepeated(2000))
	badCRC := append([]byte(nil), z...)
	badCRC[len(badCRC)-8] ^= 0xff // first byte of the CRC32 trailer
	corrupt := append([]byte(nil), z...)
	for i := 20; i < 40; i++ {
		corrupt[i] ^= 0xa5
	}
	tests := []struct {
		name string
		data []byte
		want error // nil: any non-EOF error is enough
	}{
		{"half a stream", z[:len(z)/2], io.ErrUnexpectedEOF},
		{"trailer missing", z[:len(z)-8], io.ErrUnexpectedEOF},
		{"checksum mismatch", badCRC, gzip.ErrChecksum},
		{"corrupt deflate data", corrupt, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := srcOpenLocal(t, srcWriteFile(t, "bad.gz", tt.data))

			_, err := io.ReadAll(s)

			if err == nil {
				t.Fatal("ReadAll returned a clean EOF for a damaged gzip stream")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// A file that is only the two magic bytes cannot even yield a gzip header.
func TestOpenInput_GzipHeaderTruncatedFailsToOpen(t *testing.T) {
	p := srcWriteFile(t, "magic-only.gz", []byte{0x1f, 0x8b})

	s, err := openInput(t.Context(), nil, p)

	if err == nil {
		_ = s.Close()
		t.Fatal("openInput accepted a gzip file with no header")
	}
}

func TestOpenInput_MissingFileNamesThePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.csv")

	_, err := openInput(t.Context(), nil, p)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name the path %q", err, p)
	}
}

func TestOpenInput_DirectoryIsRejected(t *testing.T) {
	d := t.TempDir()

	s, err := openInput(t.Context(), nil, d)

	if err == nil {
		_ = s.Close()
		t.Fatal("openInput accepted a directory")
	}
	if want := "input " + d + " is a directory"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// A device is not a regular file: its size is unknown, not 0.
func TestOpenInput_NonRegularFileHasUnknownSize(t *testing.T) {
	s := srcOpenLocal(t, os.DevNull)

	n, err := s.Read(make([]byte, 4))

	if n != 0 || err != io.EOF {
		t.Errorf("Read = %d, %v; want 0, io.EOF", n, err)
	}
	if s.Size() != -1 {
		t.Errorf("Size() = %d, want -1", s.Size())
	}
}

func TestOpenInput_EmptyInputIsRejected(t *testing.T) {
	if _, err := openInput(t.Context(), nil, ""); err == nil {
		t.Fatal("empty input accepted")
	}
}

// openInput does not trust parseConfig: a value with "://" is a URL and must
// be https, so file:// and http:// never reach os.Open or the network.
func TestOpenInput_NonHTTPSURLsAreRejectedWithoutOpening(t *testing.T) {
	secret := srcWriteFile(t, "x", []byte("data"))
	for _, in := range []string{
		"http://example.com/x?token=" + srcSecret,
		"file://" + secret,
		"HTTP://example.com/x",
		"ftp://example.com/x",
		"https:///nohost?token=" + srcSecret,
	} {
		s, err := openInput(t.Context(), nil, in)
		if err == nil {
			_ = s.Close()
			t.Errorf("openInput(%q) succeeded", in)
			continue
		}
		if strings.Contains(err.Error(), srcSecret) {
			t.Errorf("error for %q leaks the secret: %v", in, err)
		}
	}
}

func TestOpenInput_URLWithoutClientIsAnError(t *testing.T) {
	if _, err := openInput(t.Context(), nil, "https://example.com/x"); err == nil {
		t.Fatal("URL input with a nil client accepted")
	}
}

func TestClassifyInput(t *testing.T) {
	tests := []struct {
		in      string
		isURL   bool
		wantErr bool
	}{
		{"data/products.csv.gz", false, false},
		{"./a:b", false, false},
		{`C:\data\x.csv`, false, false},
		{"/abs/path with space.tsv", false, false},
		{"https://example.com/a.gz", true, false},
		{"HTTPS://Example.com/a.gz", true, false},
		{"https://example.com:8443/a?x=1", true, false},
		{"http://example.com/a", false, true},
		{"HTTP://example.com/a", false, true},
		{"file:///etc/passwd", false, true},
		{"ftp://example.com/a", false, true},
		{"://example.com/a", false, true},
		{"https:///nohost", false, true},
		{"https://:443/x", false, true},
		{"https://:8080", false, true},
		{"https_products.tsv", false, false},
		{"https://:" + srcSecret + "@example.com/x", false, true},
		{"https://u:" + srcSecret + "@example.com/x?token=" + srcSecret, false, true},
		{"https://user@example.com/x", false, true},
		{"https://[::1/x?token=" + srcSecret, false, true},
		{"https://example.com:badport/x?token=" + srcSecret, false, true},
		{"", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := classifyInput(tt.in)

			if (err != nil) != tt.wantErr {
				t.Fatalf("classifyInput(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), srcSecret) {
				t.Errorf("error leaks the secret: %v", err)
			}
			if err == nil && (u != nil) != tt.isURL {
				t.Errorf("classifyInput(%q) url = %v, want isURL %v", tt.in, u, tt.isURL)
			}
		})
	}
}

func TestRedactURL_KeepsSchemeHostAndPathOnly(t *testing.T) {
	u, err := url.Parse("https://u:" + srcSecret + "@Host.example:8443/a/b?token=" + srcSecret + "#frag-" + srcSecret)
	if err != nil {
		t.Fatal(err)
	}

	got := redactURL(u)

	if got != "https://Host.example:8443/a/b" {
		t.Errorf("redactURL = %q, want %q", got, "https://Host.example:8443/a/b")
	}
}

// The policy is the decoded path (it identifies the file and reads well).
func TestRedactURL_UsesTheDecodedPath(t *testing.T) {
	u, err := url.Parse("https://host.example/a%20b/c?token=" + srcSecret)
	if err != nil {
		t.Fatal(err)
	}

	if got := redactURL(u); got != "https://host.example/a b/c" {
		t.Errorf("redactURL = %q, want the decoded path", got)
	}
}

func TestWithoutRequestURL(t *testing.T) {
	leaky := "https://u:" + srcSecret + "@host/" + srcSecret + "-segment?token=" + srcSecret
	ue := &url.Error{Op: "Get", URL: leaky, Err: io.ErrUnexpectedEOF}
	if !strings.Contains(ue.Error(), srcSecret) {
		t.Fatal("fixture error should carry the secret")
	}

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"url.Error", ue, io.ErrUnexpectedEOF},
		{"wrapped url.Error", fmt.Errorf("outer: %w", ue), io.ErrUnexpectedEOF},
		{"nested url.Error", &url.Error{Op: "Get", URL: leaky, Err: ue}, io.ErrUnexpectedEOF},
		{"other error is kept", io.ErrClosedPipe, io.ErrClosedPipe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withoutRequestURL(tt.err)

			if !errors.Is(got, tt.want) {
				t.Errorf("withoutRequestURL = %v, want cause %v", got, tt.want)
			}
			if strings.Contains(got.Error(), srcSecret) || strings.Contains(got.Error(), "host") {
				t.Errorf("still carries the request URL: %q", got)
			}
		})
	}
	if withoutRequestURL(nil) != nil {
		t.Error("withoutRequestURL(nil) != nil")
	}
}
