package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// plProduct builds a product whose docCost is exactly cost: the id carries the
// sequence number, the name is padded.
func plProduct(t *testing.T, id string, cost int) catalog.Product {
	t.Helper()
	pad := cost - docOverheadBytes - len(id)
	if pad < 0 {
		t.Fatalf("cost %d too small for id %q", cost, id)
	}
	return catalog.Product{ID: id, Name: strings.Repeat("n", pad), Brands: []string{}, Categories: []string{}}
}

func TestDocCost_CountsEveryFieldPlusOverhead(t *testing.T) {
	p := catalog.Product{
		ID:         "12345",                    // 5
		Name:       "abcdefg",                  // 7
		Brands:     []string{"bb", "ccc"},      // 5
		Categories: []string{"d", "ee", "fff"}, // 6
		Nutriscore: "a",                        // 1
	}
	if got, want := docCost(p), 5+7+5+6+1+docOverheadBytes; got != want {
		t.Errorf("docCost = %d, want %d", got, want)
	}
	if got := docCost(catalog.Product{}); got != docOverheadBytes {
		t.Errorf("docCost(empty) = %d, want %d", got, docOverheadBytes)
	}
}

func TestBatchConstants(t *testing.T) {
	if batchDocs != 1000 || batchBytes != 512<<10 || docOverheadBytes != 100 || workers != 2 {
		t.Errorf("constants = %d %d %d %d, want 1000 524288 100 2", batchDocs, batchBytes, docOverheadBytes, workers)
	}
}

func plFeed(t *testing.T, products []catalog.Product) (batches [][]catalog.Product) {
	t.Helper()
	var b batcher
	for _, p := range products {
		if full := b.add(p); full != nil {
			batches = append(batches, full)
		}
	}
	if rest := b.take(); rest != nil {
		batches = append(batches, rest)
	}
	return batches
}

func plSizes(batches [][]catalog.Product) []int {
	var s []int
	for _, b := range batches {
		s = append(s, len(b))
	}
	return s
}

func plSmall(t *testing.T, n int) []catalog.Product {
	t.Helper()
	out := make([]catalog.Product, n)
	for i := range out {
		out[i] = plProduct(t, strconv.Itoa(i), 150)
	}
	return out
}

func TestBatcher_DocCountBoundaries(t *testing.T) {
	tests := []struct {
		n    int
		want []int
	}{
		{0, nil},
		{1, []int{1}},
		{batchDocs - 1, []int{999}},
		{batchDocs, []int{1000}},
		{batchDocs + 1, []int{1000, 1}},
		{2500, []int{1000, 1000, 500}},
	}
	for _, tt := range tests {
		got := plSizes(plFeed(t, plSmall(t, tt.n)))
		if !plEqualInts(got, tt.want) {
			t.Errorf("%d docs: batch sizes %v, want %v", tt.n, got, tt.want)
		}
	}
}

func TestBatcher_FullBatchIsReturnedAsSoonAsItHas1000Docs(t *testing.T) {
	var b batcher
	for i, p := range plSmall(t, batchDocs) {
		full := b.add(p)
		if (full != nil) != (i == batchDocs-1) {
			t.Fatalf("add #%d returned a batch = %v", i+1, full != nil)
		}
	}
	if rest := b.take(); rest != nil {
		t.Errorf("take after a full batch = %d docs, want none", len(rest))
	}
}

func TestBatcher_ByteCapBoundaries(t *testing.T) {
	const cost = batchBytes / 16 // 16 documents fill the cap exactly
	mk := func(n int) []catalog.Product {
		out := make([]catalog.Product, n)
		for i := range out {
			out[i] = plProduct(t, "id"+strconv.Itoa(i), cost)
		}
		return out
	}
	tests := []struct {
		name string
		in   []catalog.Product
		want []int
	}{
		{"exactly at the cap fits", mk(16), []int{16}},
		{"one more starts a new batch", mk(17), []int{16, 1}},
		{"one byte over the cap does not fit", append(mk(15), plProduct(t, "last", cost+1)), []int{15, 1}},
		{"one byte under the cap fits", append(mk(15), plProduct(t, "last", cost-1)), []int{16}},
	}
	for _, tt := range tests {
		got := plSizes(plFeed(t, tt.in))
		if !plEqualInts(got, tt.want) {
			t.Errorf("%s: batch sizes %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBatcher_SingleRecordOverTheCapGoesOutAlone(t *testing.T) {
	big := plProduct(t, "big", batchBytes+1000)
	in := []catalog.Product{plProduct(t, "a", 200), plProduct(t, "b", 200), big, plProduct(t, "c", 200)}
	batches := plFeed(t, in)
	if got, want := plSizes(batches), []int{2, 1, 1}; !plEqualInts(got, want) {
		t.Fatalf("batch sizes %v, want %v", got, want)
	}
	if batches[1][0].ID != "big" {
		t.Errorf("middle batch = %q, want the oversized record alone", batches[1][0].ID)
	}
	// Alone and last.
	got := plSizes(plFeed(t, []catalog.Product{big}))
	if !plEqualInts(got, []int{1}) {
		t.Errorf("only the big record: %v, want [1]", got)
	}
}

func TestBatcher_ByteCountResetsAfterAFlush(t *testing.T) {
	const cost = batchBytes/16 + 1 // 15 fit, the 16th does not
	in := make([]catalog.Product, 31)
	for i := range in {
		in[i] = plProduct(t, "id"+strconv.Itoa(i), cost)
	}
	if got, want := plSizes(plFeed(t, in)), []int{15, 15, 1}; !plEqualInts(got, want) {
		t.Errorf("batch sizes %v, want %v", got, want)
	}
}

func TestBatcher_EmittedBatchesNeverShareMemory(t *testing.T) {
	var b batcher
	var got [][]catalog.Product
	for _, p := range plSmall(t, 2500) {
		if full := b.add(p); full != nil {
			got = append(got, full)
		}
	}
	got = append(got, b.take())
	next := 0
	for bi, batch := range got {
		for _, p := range batch {
			if p.ID != strconv.Itoa(next) {
				t.Fatalf("batch %d holds id %q, want %d: a batch was overwritten", bi, p.ID, next)
			}
			next++
		}
	}
	if next != 2500 {
		t.Errorf("saw %d docs, want 2500", next)
	}
}

func TestBatcher_EmittedBatchesAreAllocatedWithFullCapacity(t *testing.T) {
	// One allocation per batch: appending never regrows (performance choice).
	var b batcher
	var full []catalog.Product
	for _, p := range plSmall(t, batchDocs) {
		full = b.add(p)
	}
	if cap(full) != batchDocs {
		t.Errorf("cap of a full batch = %d, want %d", cap(full), batchDocs)
	}
}
