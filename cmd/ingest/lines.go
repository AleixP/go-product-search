package main

import (
	"bufio"
	"io"
)

const (
	// maxLineBytes is the default limit on the content length of one line: 1 MiB.
	// The longest real row is 17,944 bytes (58x below), and a larger limit costs
	// peak RSS on adversarial input (the reader keeps max+1 bytes); with 1 MiB
	// and 512 KiB batches the peak stays inside the 64 MB budget.
	maxLineBytes = 1 << 20
	// readBufBytes is the size of the bufio buffer wrapped around the input.
	readBufBytes = 64 << 10
)

// lineReader reads '\n' terminated lines from a stream with memory bounded by
// the line limit, not by the line length.
//
// The limit applies to the content of a line: the bytes before the '\n',
// without one trailing '\r' (CRLF). A line of exactly the limit is accepted,
// one byte more is "too long". A '\r' that is not directly before the '\n' is
// data, and so is a '\r' at the end of the input with no '\n' after it.
type lineReader struct {
	br  *bufio.Reader
	max int
	// buf accumulates a line that does not fit in one bufio buffer. It is
	// allocated once, at the first accumulation, with the capacity of the longest
	// raw line (max content bytes plus a '\r'), so it never grows by append: growing
	// would leave garbage of about 5x the final size and push the peak RSS past
	// the memory budget.
	//
	// shortcut: after the first line that needs accumulation the reader keeps
	// max+1 bytes (~1 MiB with the default limit) for its whole life; upgrade to
	// releasing it only if that fixed cost ever matters.
	buf []byte
	err error // sticky: io.EOF or the first read error
}

// newLineReader reads lines of at most maxLine content bytes from r. A
// non-positive maxLine selects maxLineBytes.
func newLineReader(r io.Reader, maxLine int) *lineReader {
	return newLineReaderSize(r, maxLine, readBufBytes)
}

// newLineReaderSize is newLineReader with a custom bufio size (tests use small
// buffers to exercise the accumulation path with small data).
func newLineReaderSize(r io.Reader, maxLine, size int) *lineReader {
	if maxLine <= 0 {
		maxLine = maxLineBytes
	}
	return &lineReader{br: bufio.NewReaderSize(r, size), max: maxLine}
}

// next returns the next line without its terminator. Empty lines are returned
// as empty (non-nil) slices; the caller decides what to do with them.
//
// A line longer than the limit is consumed up to its '\n' without keeping it:
// next returns a nil line and tooLong == true, and the following call returns
// the next line normally.
//
// The returned slice is only valid until the next call to next.
//
// The last line may have no '\n'; it is returned normally and the next call
// returns io.EOF. Any other error from the underlying reader is returned as is
// and immediately (io.ErrUnexpectedEOF stays io.ErrUnexpectedEOF); a partial
// line, including data that arrived together with that error, is dropped and
// never returned as a complete line. Errors are sticky.
func (l *lineReader) next() (line []byte, tooLong bool, err error) {
	if l.err != nil {
		return nil, false, l.err
	}
	l.buf = l.buf[:0]
	skipping := false
	for {
		chunk, err := l.br.ReadSlice('\n')
		complete := err == nil
		switch err {
		case nil, bufio.ErrBufferFull:
		case io.EOF:
			l.err = io.EOF
			if len(chunk) == 0 && len(l.buf) == 0 && !skipping {
				return nil, false, io.EOF
			}
		default:
			l.err = err
			return nil, false, err
		}
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		last := complete || l.err != nil // no more bytes belong to this line

		// A raw length above max+1 cannot fit even after trimming one '\r'.
		if !skipping && len(l.buf)+len(chunk) > l.max+1 {
			skipping = true
		}
		if skipping {
			if last {
				return nil, true, nil
			}
			continue
		}
		if !last {
			if l.buf == nil {
				l.buf = make([]byte, 0, l.max+1)
			}
			l.buf = append(l.buf, chunk...)
			continue
		}
		if len(l.buf) == 0 {
			line = chunk // fast path: no copy, points into the bufio buffer
		} else {
			l.buf = append(l.buf, chunk...)
			line = l.buf
		}
		if complete && len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > l.max {
			return nil, true, nil
		}
		return line, false, nil
	}
}
