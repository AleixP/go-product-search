package main

import "github.com/AleixP/go-product-search/internal/catalog"

// Batching and concurrency limits of the pipeline.
const (
	// batchDocs is the most documents in one bulk request.
	batchDocs = 1000
	// batchBytes is the most bytes of one bulk request, counted as field bytes plus
	// docOverheadBytes per document (see docCost). Why 512 KiB and not 2 MiB: a
	// control byte becomes 6 bytes of JSON, so the bulk body is up to 6x the field
	// bytes. In a worst-case model (1,000 records of the field caps filled with
	// control bytes) a 2 MiB cap peaked at 65-80 MB of RSS, over the 64 MB budget;
	// 512 KiB peaked at 28-32 MB. Real data hits batchDocs long before this cap.
	batchBytes = 512 << 10
	// docOverheadBytes is the fixed cost added per document: the action line,
	// the field names and the JSON punctuation.
	docOverheadBytes = 100
	// workers is the number of bulk requests in flight at once. With the batch
	// being built there are at most workers+1 batches in memory.
	workers = 2
)

// docCost is the size charged to a document against batchBytes: the bytes of
// its text fields plus docOverheadBytes.
func docCost(p catalog.Product) int {
	n := len(p.ID) + len(p.Name) + len(p.Nutriscore) + docOverheadBytes
	for _, b := range p.Brands {
		n += len(b)
	}
	for _, c := range p.Categories {
		n += len(c)
	}
	return n
}

// batcher groups documents into bulk batches. It is used by one goroutine.
//
// Every emitted batch is a fresh slice that the batcher never touches again, so
// it can be handed to a goroutine and outlive the next batch. Simplest correct
// choice: a batch is at most 1,000 documents (about 96 KiB of headers), cheap
// next to the bulk body, and reuse would need to wait for the request to end.
type batcher struct {
	cur   []catalog.Product
	bytes int
}

// add appends p and returns a batch that is ready to send, or nil. A batch is
// ready when it reaches batchDocs documents (returned with p inside) or when p
// would push it over batchBytes (returned without p, which starts the next
// batch). A single record over the cap therefore goes out alone: the next add
// (or take) emits it.
func (b *batcher) add(p catalog.Product) []catalog.Product {
	cost := docCost(p)
	var full []catalog.Product
	if len(b.cur) > 0 && b.bytes+cost > batchBytes {
		full = b.take()
	}
	if b.cur == nil {
		b.cur = make([]catalog.Product, 0, batchDocs)
	}
	b.cur = append(b.cur, p)
	b.bytes += cost
	if len(b.cur) == batchDocs {
		return b.take()
	}
	return full
}

// take returns the documents collected so far (nil when there are none) and
// starts a new empty batch.
func (b *batcher) take() []catalog.Product {
	out := b.cur
	b.cur, b.bytes = nil, 0
	return out
}
