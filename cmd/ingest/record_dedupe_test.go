package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// recDistinctItems returns n distinct two-byte items, comma separated.
func recDistinctItems(n int, prefix string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		// 0x21..0x7e minus the comma: 93 symbols, 8,649 distinct pairs.
		x, y := byte(0x21+i/93%93), byte(0x21+i%93)
		if x == ',' {
			x = 0x7f
		}
		if y == ',' {
			y = 0x7f
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(prefix)
		b.WriteByte(x)
		b.WriteByte(y)
	}
	return b.String()
}

// The densest list a row can hold under the size cap: two-byte distinct items.
// The duplicate check must stay cheap (it switches to a set past 32 items).
func TestDecode_ManyDistinctItemsStayLinear(t *testing.T) {
	const n = (maxListFieldBytes - 3 + 1) / 3 // 5,460 items of 2 bytes plus a comma
	const m = (maxListFieldBytes - 6 + 1) / 6 // 2,729 items of "en:" and 2 bytes plus a comma
	brands := recDistinctItems(n, "")
	cats := recDistinctItems(m, "en:")
	if len(brands)+3 > maxListFieldBytes || len(cats)+6 > maxListFieldBytes {
		t.Fatalf("fixture too big: %d and %d bytes, cap %d", len(brands)+3, len(cats)+6, maxListFieldBytes)
	}
	line := recFields("1", "N", brands+",\x21\x21", cats+",en:\x21\x21", "en:us", "a") // the last item repeats the first

	h := recStd(t)
	b := []byte(line)
	// The best of a few runs: a loaded machine slows single runs, not all of
	// them. With the set a run takes about 1 ms (5 ms under -race); comparing
	// every item with every other takes about 35 ms (more under -race).
	var elapsed time.Duration
	var got record
	for i := 0; i < 5; i++ {
		start := time.Now()
		var reason skipReason
		got, reason = h.decode(b)
		if d := time.Since(start); i == 0 || d < elapsed {
			elapsed = d
		}
		if reason != skipNone {
			t.Fatalf("decode reason = %v, want none", reason)
		}
	}

	if len(got.brands) != n || len(got.categories) != m {
		t.Fatalf("len(brands) = %d, len(categories) = %d, want %d and %d", len(got.brands), len(got.categories), n, m)
	}
	if got.brands[0] != "\x21\x21" || got.brands[n-1] != brands[len(brands)-2:] {
		t.Errorf("order not kept: first %q last %q", got.brands[0], got.brands[n-1])
	}
	if elapsed > 15*time.Millisecond {
		t.Errorf("decoding %d distinct items took %v at best, want under 15ms (a quadratic duplicate check takes about 35ms)", n, elapsed)
	}
}

func TestDecode_DedupeAroundTheLinearLimit(t *testing.T) {
	// Sizes straddle the point where the duplicate check changes strategy.
	for _, n := range []int{1, 2, 15, 16, 17, 31, 32, 33, 34, 64, 100} {
		var items, wantItems []string
		for i := 0; i < n; i++ {
			items = append(items, fmt.Sprintf("b%d", i))
			wantItems = append(wantItems, fmt.Sprintf("b%d", i))
		}
		// Repeat every item twice, interleaved: b0 b1 .. b0 b1 .. and again,
		// then two items that differ only in case, lower case first: both stay.
		in := strings.Join(append(append(items, items...), items...), ",") + ",x,X,X,x"
		wantItems = append(wantItems, "x", "X")

		got := recOK(t, recFields("1", "N", in, "en:c", "en:us", "a"))

		recWantList(t, fmt.Sprintf("brands (n=%d)", n), got.brands, wantItems)
	}
}
