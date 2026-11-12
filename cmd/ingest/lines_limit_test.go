package main

import (
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// boundaryCases builds the inputs around the limit m. The limit applies to the
// content length: without the "\n" and without one trailing "\r" before it.
func boundaryCases(m int) []struct {
	name string
	in   string
	want []lineResult
} {
	a := func(n int) string { return strings.Repeat("a", n) }
	return []struct {
		name string
		in   string
		want []lineResult
	}{
		{"limit-1 accepted", a(m-1) + "\nnext\n", []lineResult{lineOK(a(m - 1)), lineOK("next")}},
		{"limit accepted", a(m) + "\nnext\n", []lineResult{lineOK(a(m)), lineOK("next")}},
		{"limit+1 skipped", a(m+1) + "\nnext\n", []lineResult{lineTooLong, lineOK("next")}},
		{"limit+2 skipped", a(m+2) + "\nnext\n", []lineResult{lineTooLong, lineOK("next")}},
		{"limit with crlf accepted", a(m) + "\r\nnext\n", []lineResult{lineOK(a(m)), lineOK("next")}},
		{"limit+1 with crlf skipped", a(m+1) + "\r\nnext\n", []lineResult{lineTooLong, lineOK("next")}},
		{"limit-1 with crlf accepted", a(m-1) + "\r\nnext\n", []lineResult{lineOK(a(m - 1)), lineOK("next")}},
		{"limit plus two cr skipped", a(m) + "\r\r\nnext\n", []lineResult{lineTooLong, lineOK("next")}},
		{"limit-1 plus lone cr kept as data", a(m-1) + "\r\r\nnext\n", []lineResult{lineOK(a(m-1) + "\r"), lineOK("next")}},
		{"limit at EOF without newline accepted", "x\n" + a(m), []lineResult{lineOK("x"), lineOK(a(m))}},
		{"limit+1 at EOF without newline skipped", "x\n" + a(m+1), []lineResult{lineOK("x"), lineTooLong}},
		{"limit plus cr at EOF skipped", "x\n" + a(m) + "\r", []lineResult{lineOK("x"), lineTooLong}},
		{"limit-1 plus cr at EOF accepted", "x\n" + a(m-1) + "\r", []lineResult{lineOK("x"), lineOK(a(m-1) + "\r")}},
		{"two skipped in a row then a good line", a(m+5) + "\n" + a(2*m) + "\r\n" + "ok\n", []lineResult{lineTooLong, lineTooLong, lineOK("ok")}},
		{"skipped at the very start of input", a(m + 1), []lineResult{lineTooLong}},
	}
}

func TestLineReader_LimitBoundary(t *testing.T) {
	for _, m := range []int{40, 1 << 10} {
		for _, size := range []int{16, 17, 64, 1000, 64 << 10} {
			for _, tt := range boundaryCases(m) {
				t.Run(strings.Join([]string{strconv.Itoa(m), strconv.Itoa(size), tt.name}, "/"), func(t *testing.T) {
					got, err := lineReadAll(t, newLineReaderSize(strings.NewReader(tt.in), m, size))
					if err != io.EOF {
						t.Fatalf("final error = %v, want io.EOF", err)
					}
					lineCheckResults(t, got, tt.want)
				})
			}
		}
	}
}

func TestLineReader_LimitBoundaryRealLimit(t *testing.T) {
	for _, tt := range boundaryCases(maxLineBytes) {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lineReadAll(t, newLineReader(strings.NewReader(tt.in), maxLineBytes))
			if err != io.EOF {
				t.Fatalf("final error = %v, want io.EOF", err)
			}
			lineCheckResults(t, got, tt.want)
		})
	}
}

func TestLineReader_DefaultLimitIs1MiB(t *testing.T) {
	if maxLineBytes != 1<<20 {
		t.Fatalf("maxLineBytes = %d, want %d", maxLineBytes, 1<<20)
	}
	if readBufBytes != 64<<10 {
		t.Fatalf("readBufBytes = %d, want %d", readBufBytes, 64<<10)
	}
	lr := newLineReader(strings.NewReader(""), 0)
	if lr.br.Size() != 64<<10 || lr.max != 1<<20 {
		t.Fatalf("newLineReader uses buffer %d and limit %d, want 64 KiB and 1 MiB", lr.br.Size(), lr.max)
	}
}

// xThenTail yields n 'x' bytes without holding them in memory, then tail.
type xThenTail struct {
	n    int64
	tail strings.Reader
}

func (x *xThenTail) Read(p []byte) (int, error) {
	if x.n == 0 {
		return x.tail.Read(p)
	}
	k := int64(len(p))
	if k > x.n {
		k = x.n
	}
	for i := range p[:k] {
		p[i] = 'x'
	}
	x.n -= k
	return int(k), nil
}

func TestLineReader_HugeLineDoesNotGrowMemory(t *testing.T) {
	const limit = 1 << 10
	const lineLen = 64 << 20 // 65536 times the limit
	src := &xThenTail{n: lineLen}
	src.tail = *strings.NewReader("\nnext\n")
	lr := newLineReader(src, limit)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := lineReadAll(t, lr)
	runtime.ReadMemStats(&after)

	if err != io.EOF {
		t.Fatal(err)
	}
	lineCheckResults(t, got, []lineResult{lineTooLong, lineOK("next")})
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 256<<10 {
		t.Fatalf("allocated %d bytes reading a %d byte line with limit %d, want < 256 KiB", grown, lineLen, limit)
	}
}

func TestLineReader_ManyConsecutiveTooLongLines(t *testing.T) {
	const limit = 1 << 10
	var sb strings.Builder
	const n = 500
	for i := 0; i < n; i++ {
		sb.WriteString(strings.Repeat("a", limit+1+i*7))
		sb.WriteString("\n")
	}
	sb.WriteString("end\n")
	got, err := lineReadAll(t, newLineReaderSize(strings.NewReader(sb.String()), limit, 64))
	if err != io.EOF {
		t.Fatal(err)
	}
	want := make([]lineResult, 0, n+1)
	for i := 0; i < n; i++ {
		want = append(want, lineTooLong)
	}
	want = append(want, lineOK("end"))
	lineCheckResults(t, got, want)
}

// repeatReader is an endless source of the same bytes.
type repeatReader struct {
	pattern []byte
	pos     int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.pattern[r.pos:])
		n += c
		r.pos = (r.pos + c) % len(r.pattern)
	}
	return n, nil
}

func line3KB() []byte {
	return []byte(strings.Repeat("c\t", 1500) + "\n")
}

// TestLineReader_NoMallocsPerLineInSteadyState counts mallocs over a fixed
// number of lines (AllocsPerRun rounds down, so it would hide one allocation per
// few hundred lines). The first lines warm up the reusable buffer.
func TestLineReader_NoMallocsPerLineInSteadyState(t *testing.T) {
	tests := []struct {
		name    string
		pattern []byte
		limit   int
		lines   int
	}{
		{"3 KB lines in the buffer", line3KB(), maxLineBytes, 20_000},
		{"3 KB crlf lines", []byte(strings.Repeat("c", 3000) + "\r\n"), maxLineBytes, 20_000},
		{"lines needing accumulation", []byte(strings.Repeat("d", 150_000) + "\n"), maxLineBytes, 5_000},
		{"accumulation, last chunk large", []byte(strings.Repeat("d", 131_072+40_000) + "\n"), maxLineBytes, 5_000},
		{"accumulation, 65 KB", []byte(strings.Repeat("d", 65_000) + "\n"), maxLineBytes, 5_000},
		{"too-long lines being discarded", []byte(strings.Repeat("e", 150_000) + "\n"), 1 << 10, 5_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newLineReader(&repeatReader{pattern: tt.pattern}, tt.limit)
			for i := 0; i < 10; i++ {
				if _, _, err := lr.next(); err != nil {
					t.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := 0; i < tt.lines; i++ {
				if _, _, err := lr.next(); err != nil {
					t.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			// The runtime itself may account for a stray malloc or two (observed: 1)
			// while the test runs; an allocation per 500 lines would be 10+.
			if m := after.Mallocs - before.Mallocs; m > 3 {
				t.Fatalf("%d mallocs over %d lines, want ~0 (at most 3 of runtime noise)", m, tt.lines)
			}
		})
	}
}

// totalAllocReading returns the bytes allocated while reading everything from lr.
func totalAllocReading(t *testing.T, lr *lineReader) ([]lineResult, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := lineReadAll(t, lr)
	runtime.ReadMemStats(&after)
	if err != io.EOF {
		t.Fatal(err)
	}
	return got, after.TotalAlloc - before.TotalAlloc
}

// With a limit above the bufio size every chunk fits the limit, so only the
// running total (buffer + chunk) can stop the accumulation of a huge line.
func TestLineReader_HugeLineBoundedWhenChunksFitLimit(t *testing.T) {
	const limit = 1 << 20
	src := &xThenTail{n: 64 << 20} // 64 times the limit
	src.tail = *strings.NewReader("\nnext\n")

	got, grown := totalAllocReading(t, newLineReader(src, limit))

	lineCheckResults(t, got, []lineResult{lineTooLong, lineOK("next")})
	// The bufio buffer plus one accumulation buffer of the limit is ~1.1 MiB.
	if grown > 3*limit+1<<20 {
		t.Fatalf("allocated %d bytes for a 64 MiB line with limit 1 MiB, want < %d", grown, 3*limit+1<<20)
	}
}

// A line of exactly the limit must be assembled in one buffer allocated once
// at its final size: growing it by append would allocate ~5x the limit.
func TestLineReader_LongAcceptedLineAllocatesBufferOnce(t *testing.T) {
	const limit = 4 << 20
	src := &xThenTail{n: limit}
	src.tail = *strings.NewReader("\n")
	lr := newLineReader(src, limit)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	line, tooLong, err := lr.next() // not copied: only the reader's own allocations count
	runtime.ReadMemStats(&after)

	if err != nil || tooLong || len(line) != limit {
		t.Fatalf("next() = %d bytes, %v, %v; want %d bytes", len(line), tooLong, err, limit)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > limit+limit/2 {
		t.Fatalf("allocated %d bytes for one %d byte line, want < %d", grown, limit, limit+limit/2)
	}
}

// The accumulation buffer is allocated at the first accumulation (normal inputs
// never pay for it) with the capacity of the longest raw line (limit content
// bytes plus one '\r'), and never grows after that.
func TestLineReader_AccumulationBufferIsLazyAndFixed(t *testing.T) {
	const limit = 1000
	a := func(n int) string { return strings.Repeat("a", n) }
	lr := newLineReaderSize(strings.NewReader(a(10)+"\n"+a(limit+1)+"\n"+a(limit)+"\r\n"+a(limit)+"\n"+a(limit+1)+"\r\n"), limit, 16)

	if cap(lr.buf) != 0 {
		t.Fatalf("buffer allocated at construction: cap %d", cap(lr.buf))
	}
	if _, _, err := lr.next(); err != nil {
		t.Fatal(err)
	}
	if cap(lr.buf) != 0 {
		t.Fatalf("buffer allocated by a line that fits the bufio buffer: cap %d", cap(lr.buf))
	}
	for i := 0; i < 4; i++ { // too long, limit+CRLF, limit+LF, limit+1 CRLF (raw limit+2: skipped, not stored)
		if _, _, err := lr.next(); err != nil {
			t.Fatal(err)
		}
		if cap(lr.buf) != limit+1 {
			t.Fatalf("after call %d: cap(buf) = %d, want %d", i+2, cap(lr.buf), limit+1)
		}
	}
}

// An accumulated line whose reader returns data after its EOF must not be read
// again: EOF is sticky even though strings.Reader would repeat it anyway.
type eofThenData struct{ stage int }

func (e *eofThenData) Read(p []byte) (int, error) {
	switch e.stage {
	case 0:
		e.stage = 1
		return copy(p, "a\n"), nil
	case 1:
		e.stage = 2
		return 0, io.EOF
	default:
		return copy(p, "b\n"), nil
	}
}

func TestLineReader_EOFIsStickyWhenReaderYieldsDataAfterEOF(t *testing.T) {
	lr := newLineReader(&eofThenData{}, 1<<10)
	if line, _, err := lr.next(); err != nil || string(line) != "a" {
		t.Fatalf("first line %q %v", line, err)
	}
	for i := 0; i < 3; i++ {
		line, tooLong, err := lr.next()
		if err != io.EOF || line != nil || tooLong {
			t.Fatalf("call %d after EOF: %q %v %v, want nil false io.EOF", i, line, tooLong, err)
		}
	}
}

func BenchmarkLineReader3KB(b *testing.B) {
	lr := newLineReader(&repeatReader{pattern: line3KB()}, maxLineBytes)
	b.ReportAllocs()
	b.SetBytes(3001)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := lr.next(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLineReaderAccumulate(b *testing.B) {
	pattern := []byte(strings.Repeat("d", 150_000) + "\n") // longer than the 64 KiB bufio buffer
	lr := newLineReader(&repeatReader{pattern: pattern}, maxLineBytes)
	b.ReportAllocs()
	b.SetBytes(int64(len(pattern)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := lr.next(); err != nil {
			b.Fatal(err)
		}
	}
}

// A too-long line without a newline whose length is a multiple of the bufio
// size ends with an empty read at EOF; it must still be reported as too long.
func TestLineReader_TooLongEndingOnBufferBoundaryAtEOF(t *testing.T) {
	const limit = 40
	for _, size := range []int{16, 32, 64} {
		for n := size; n <= 8*size; n += size {
			if n <= limit+1 {
				continue
			}
			for _, prefix := range []string{"", "x\n"} {
				want := []lineResult{lineTooLong}
				if prefix != "" {
					want = []lineResult{lineOK("x"), lineTooLong}
				}
				got, err := lineReadAll(t, newLineReaderSize(strings.NewReader(prefix+strings.Repeat("a", n)), limit, size))
				if err != io.EOF {
					t.Fatalf("size %d n %d: error %v, want io.EOF", size, n, err)
				}
				lineCheckResults(t, got, want)
			}
		}
	}
}

// With the default limit a 1 MiB line is accepted, one byte more is skipped,
// and the single accumulation buffer is max+1 bytes (~1 MiB).
func TestLineReader_DefaultLimitBehaviour(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	in := a(maxLineBytes) + "\n" + a(maxLineBytes+1) + "\nnext\n"
	lr := newLineReader(strings.NewReader(in), 0)

	got, err := lineReadAll(t, lr)

	if err != io.EOF {
		t.Fatal(err)
	}
	lineCheckResults(t, got, []lineResult{lineOK(a(maxLineBytes)), lineTooLong, lineOK("next")})
	if cap(lr.buf) != maxLineBytes+1 {
		t.Fatalf("cap(buf) = %d, want %d", cap(lr.buf), maxLineBytes+1)
	}
}
