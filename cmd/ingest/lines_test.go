package main

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// lineResult is one next() outcome with the line copied out of the reader's buffer.
type lineResult struct {
	line    string
	tooLong bool
}

// lineReadAll calls next until it returns an error and returns the results and that error.
func lineReadAll(t *testing.T, lr *lineReader) ([]lineResult, error) {
	t.Helper()
	var out []lineResult
	for i := 0; i < 1_000_000; i++ {
		line, tooLong, err := lr.next()
		if err != nil {
			if line != nil || tooLong {
				t.Fatalf("next() returned line=%q tooLong=%v together with error %v", line, tooLong, err)
			}
			return out, err
		}
		if tooLong && len(line) != 0 {
			t.Fatalf("next() returned %d bytes for a too-long line, want none", len(line))
		}
		out = append(out, lineResult{line: string(line), tooLong: tooLong})
	}
	t.Fatal("lineReadAll did not terminate")
	return nil, nil
}

// lineCheckResults compares two result lists without printing huge lines.
func lineCheckResults(t *testing.T, got, want []lineResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %s vs %s", len(got), len(want), lineBrief(got), lineBrief(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %s, want %s", i, lineBrief(got[i:i+1]), lineBrief(want[i:i+1]))
		}
	}
}

func lineBrief(rs []lineResult) string {
	var sb strings.Builder
	for _, r := range rs {
		s := r.line
		if len(s) > 12 {
			s = s[:12] + "..."
		}
		sb.WriteString("[")
		sb.WriteString(strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s))
		sb.WriteString("|len=")
		sb.WriteString(strconv.Itoa(len(r.line)))
		if r.tooLong {
			sb.WriteString("|TOOLONG")
		}
		sb.WriteString("]")
	}
	return sb.String()
}

func lineOK(s string) lineResult { return lineResult{line: s} }

var lineTooLong = lineResult{tooLong: true}

func TestLineReader_Basics(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []lineResult
	}{
		{"empty input", "", nil},
		{"one line with newline", "abc\n", []lineResult{lineOK("abc")}},
		{"one line without newline", "abc", []lineResult{lineOK("abc")}},
		{"several lines", "a\nbb\nccc\n", []lineResult{lineOK("a"), lineOK("bb"), lineOK("ccc")}},
		{"several lines last without newline", "a\nbb\nccc", []lineResult{lineOK("a"), lineOK("bb"), lineOK("ccc")}},
		{"crlf trimmed", "a\r\nbb\r\n", []lineResult{lineOK("a"), lineOK("bb")}},
		{"only one trailing cr trimmed", "a\r\r\n", []lineResult{lineOK("a\r")}},
		{"lone cr in the middle is data", "a\rb\nc\n", []lineResult{lineOK("a\rb"), lineOK("c")}},
		{"cr without newline at EOF is data", "a\r", []lineResult{lineOK("a\r")}},
		{"blank lines are returned as empty lines", "a\n\n\r\nb\n", []lineResult{lineOK("a"), lineOK(""), lineOK(""), lineOK("b")}},
		{"only a newline", "\n", []lineResult{lineOK("")}},
		{"tabs are data", "a\tb\tc\n", []lineResult{lineOK("a\tb\tc")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lineReadAll(t, newLineReader(strings.NewReader(tt.in), 1<<10))
			if err != io.EOF {
				t.Fatalf("final error = %v, want io.EOF", err)
			}
			lineCheckResults(t, got, tt.want)
		})
	}
}

func TestLineReader_EOFIsSticky(t *testing.T) {
	lr := newLineReader(strings.NewReader("a\n"), 1<<10)
	if _, _, err := lr.next(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if line, tooLong, err := lr.next(); err != io.EOF || line != nil || tooLong {
			t.Fatalf("call %d after the end: %q %v %v, want nil false io.EOF", i, line, tooLong, err)
		}
	}
}

func TestLineReader_NonPositiveMaxUsesDefault(t *testing.T) {
	for _, maxLine := range []int{0, -1} {
		got, err := lineReadAll(t, newLineReader(strings.NewReader(strings.Repeat("a", 100)+"\n"), maxLine))
		if err != io.EOF {
			t.Fatal(err)
		}
		lineCheckResults(t, got, []lineResult{lineOK(strings.Repeat("a", 100))})
	}
}

func TestLineReader_ReassemblesLineLargerThanDefaultBuffer(t *testing.T) {
	// 3 x the 64 KiB bufio buffer plus a bit, with the default-size reader.
	a := strings.Repeat("a", 200_000)
	in := a + "\n" + "next\n" + a + "\r\n" + a
	got, err := lineReadAll(t, newLineReader(strings.NewReader(in), 1<<20))
	if err != io.EOF {
		t.Fatal(err)
	}
	lineCheckResults(t, got, []lineResult{lineOK(a), lineOK("next"), lineOK(a), lineOK(a)})
}

func TestLineReader_SameLinesWithAwkwardReaders(t *testing.T) {
	in := "first\r\n\nsecond line\rwith cr\n" + strings.Repeat("x", 300) + "\n" +
		strings.Repeat("y", 50) + "\n" + "last"
	want := []lineResult{
		lineOK("first"), lineOK(""), lineOK("second line\rwith cr"), lineOK(strings.Repeat("x", 300)),
		lineOK(strings.Repeat("y", 50)), lineOK("last"),
	}
	wrappers := map[string]func(io.Reader) io.Reader{
		"plain":         func(r io.Reader) io.Reader { return r },
		"one byte":      iotest.OneByteReader,
		"half":          iotest.HalfReader,
		"data with EOF": iotest.DataErrReader,
	}
	for name, wrap := range wrappers {
		for _, size := range []int{16, 17, 64, 4096} {
			t.Run(name+"/"+strconv.Itoa(size), func(t *testing.T) {
				// max 100 makes the 300-byte line too long and keeps the others.
				got, err := lineReadAll(t, newLineReaderSize(wrap(strings.NewReader(in)), 100, size))
				if err != io.EOF {
					t.Fatalf("final error = %v, want io.EOF", err)
				}
				w := append([]lineResult(nil), want...)
				w[3] = lineTooLong
				lineCheckResults(t, got, w)
			})
		}
	}
}

var errLineBoom = errors.New("boom")

// scriptReader returns each chunk's data and error in a single Read call.
type scriptReader struct {
	chunks []scriptChunk
}

type scriptChunk struct {
	data string
	err  error
}

func (s *scriptReader) Read(p []byte) (int, error) {
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	c := &s.chunks[0]
	n := copy(p, c.data)
	c.data = c.data[n:]
	if c.data != "" {
		return n, nil
	}
	err := c.err
	s.chunks = s.chunks[1:]
	return n, err
}

func TestLineReader_ErrorsSurfaceAsIs(t *testing.T) {
	tests := []struct {
		name   string
		chunks []scriptChunk
		size   int
		want   []lineResult
		err    error
	}{
		{"unexpected EOF after full lines", []scriptChunk{{"a\nb\n", io.ErrUnexpectedEOF}}, 4096, []lineResult{lineOK("a"), lineOK("b")}, io.ErrUnexpectedEOF},
		{"unexpected EOF mid line drops the partial line", []scriptChunk{{"a\nb", io.ErrUnexpectedEOF}}, 4096, []lineResult{lineOK("a")}, io.ErrUnexpectedEOF},
		{"arbitrary error mid line drops the partial line", []scriptChunk{{"a\npartial", errLineBoom}}, 4096, []lineResult{lineOK("a")}, errLineBoom},
		{"error on its own after data", []scriptChunk{{"a\nb", nil}, {"", errLineBoom}}, 4096, []lineResult{lineOK("a")}, errLineBoom},
		{"error at the start", []scriptChunk{{"", errLineBoom}}, 4096, nil, errLineBoom},
		{"error while accumulating a long line", []scriptChunk{{strings.Repeat("a", 100), errLineBoom}}, 16, nil, errLineBoom},
		{"error while discarding a too-long line", []scriptChunk{{strings.Repeat("a", 100), nil}, {strings.Repeat("a", 100), errLineBoom}}, 16, nil, errLineBoom},
		{"error after a too-long line was skipped", []scriptChunk{{strings.Repeat("a", 100) + "\nb", errLineBoom}}, 16, []lineResult{lineTooLong}, errLineBoom},
		{"io.EOF with data is a clean last line", []scriptChunk{{"a\nb", io.EOF}}, 4096, []lineResult{lineOK("a"), lineOK("b")}, io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newLineReaderSize(&scriptReader{chunks: tt.chunks}, 40, tt.size)
			got, err := lineReadAll(t, lr)
			if err != tt.err { // identity: the error must not be wrapped or mapped
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			lineCheckResults(t, got, tt.want)
			// The error is sticky and never turns into io.EOF.
			if _, _, err := lr.next(); err != tt.err {
				t.Fatalf("second call error = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestLineReader_UnexpectedEOFFromReaderIsNotEOF(t *testing.T) {
	r := io.MultiReader(strings.NewReader("a\npartial"), iotest.ErrReader(io.ErrUnexpectedEOF))
	got, err := lineReadAll(t, newLineReader(r, 1<<10))
	if !errors.Is(err, io.ErrUnexpectedEOF) || err == io.EOF {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
	lineCheckResults(t, got, []lineResult{lineOK("a")})
}

func TestLineReader_LineValidUntilNextCall(t *testing.T) {
	for _, size := range []int{16, 4096} { // 16 forces the accumulation path
		in := "short\n" + strings.Repeat("z", 90) + "\nthird\n"
		lr := newLineReaderSize(strings.NewReader(in), 1<<10, size)
		line, _, err := lr.next()
		if err != nil {
			t.Fatal(err)
		}
		if string(line) != "short" {
			t.Fatalf("size %d: first line %q", size, line)
		}
		line, _, err = lr.next()
		if err != nil || !bytes.Equal(line, bytes.Repeat([]byte("z"), 90)) {
			t.Fatalf("size %d: second line %q err %v", size, line, err)
		}
		// Still intact right before the next call, and usable as a sub-slice.
		if string(line[:3]) != "zzz" {
			t.Fatalf("size %d: line changed before the next call", size)
		}
	}
}

func TestLineReader_EmptyLineIsNonNilEmptySlice(t *testing.T) {
	lr := newLineReader(strings.NewReader("\n\r\nx"), 1<<10)
	for i := 0; i < 2; i++ {
		line, tooLong, err := lr.next()
		if err != nil || tooLong || line == nil || len(line) != 0 {
			t.Fatalf("blank line %d: %v %v %v, want empty non-nil slice", i, line, tooLong, err)
		}
	}
}
