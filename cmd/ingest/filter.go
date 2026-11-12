package main

import (
	"hash/fnv"
	"slices"
)

// The quality filters, in precedence order. Their index is also the index of
// the pass flags returned by filters.check and of the independent pass counts.
const (
	filterCountry = iota
	filterBrand
	filterCategory
	filterNutriscore
	numFilters
)

// filterReasons maps a filter to the skip bucket of the rows it rejects.
var filterReasons = [numFilters]skipReason{skipCountry, skipBrand, skipCategory, skipNutriscore}

// filters is the sampler plus the quality filters of one run. The zero value
// keeps every row and applies no filter.
type filters struct {
	sampleEvery int
	countries   []string // taxonomy tags with prefix, empty means no country filter
	brand       bool
	category    bool
	nutriscore  bool
}

// newFilters builds the filters from a validated config. A nil or empty
// Countries/Require list turns the filter off.
func newFilters(cfg config) filters {
	return filters{
		sampleEvery: cfg.SampleEvery,
		countries:   cfg.Countries,
		brand:       slices.Contains(cfg.Require, "brand"),
		category:    slices.Contains(cfg.Require, "category"),
		nutriscore:  slices.Contains(cfg.Require, "nutriscore"),
	}
}

// enabled reports whether filter i (filterCountry...) is on.
func (f filters) enabled(i int) bool {
	switch i {
	case filterCountry:
		return len(f.countries) > 0
	case filterBrand:
		return f.brand
	case filterCategory:
		return f.category
	case filterNutriscore:
		return f.nutriscore
	}
	return false
}

// fnv1a64 is the base of the sampling hash: FNV-1a, 64 bits, over the bytes of
// s. It is stable across runs and Go versions, so the same code is kept or
// dropped in every nightly file. It is never used alone (see sampled).
func fnv1a64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// mix64 is the 64-bit finalizer of MurmurHash3 (fmix64). It makes every input
// bit change every output bit with probability about one half.
func mix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// sampled reports whether a code is kept: mix64(fnv1a64(code)) % sampleEvery
// == 0. With sampleEvery <= 1 (also the zero value) everything is kept.
//
// The hash is FNV-1a 64 bit, then mixed. FNV-1a multiplies
// by an odd prime, so the lowest bit of the hash is 1 xor the parity of the
// number of odd bytes. A valid GS1 code (EAN-13, UPC-A, EAN-8) has a check
// digit that makes its digit sum even, so the lowest bit of its hash is always
// 1 and "hash % k" with an even k kept almost nothing (on the real CSV prefix
// the plain hash kept 0.03% at k=8, and 0 of 80,000 valid EAN-13 codes at
// k=2, 4, 8 and 16; the mixed one keeps about 12.5% at k=8). mix64 spreads
// every input bit over the low bits
// and removes that structure. It must stay before the modulus, and it is
// pinned by literals in the tests: changing it changes which products an
// existing index holds after the next nightly run.
func (f filters) sampled(code string) bool {
	if f.sampleEvery <= 1 {
		return true
	}
	return mix64(fnv1a64(code))%uint64(f.sampleEvery) == 0
}

// check judges a record that decoded fine. It returns the bucket of the row:
// skipNotSampled first, then the first failing enabled filter in the order
// country, brand, category, nutriscore, or skipNone when the row passes.
//
// pass says, per filter, whether the row passes it regardless of the others;
// it is true only for an enabled filter that the row passes, and all false for
// an unsampled row. The report counts these independently of the buckets.
func (f filters) check(rec record) (skipReason, [numFilters]bool) {
	var pass [numFilters]bool
	if !f.sampled(rec.code) {
		return skipNotSampled, pass
	}
	pass[filterCountry] = f.enabled(filterCountry) && f.anyCountry(rec.countries)
	pass[filterBrand] = f.brand && len(rec.brands) >= 1
	pass[filterCategory] = f.category && len(rec.categories) >= 1
	pass[filterNutriscore] = f.nutriscore && validGrade(rec.nutriscore)
	for i := range numFilters {
		if f.enabled(i) && !pass[i] {
			return filterReasons[i], pass
		}
	}
	return skipNone, pass
}

// anyCountry reports whether one of the record's tags is in the wanted list.
// The compare is exact and case sensitive on the whole taxonomy tag.
func (f filters) anyCountry(tags []string) bool {
	for _, t := range tags {
		if slices.Contains(f.countries, t) {
			return true
		}
	}
	return false
}

// validGrade reports whether g is a Nutri-Score grade a to e.
func validGrade(g string) bool {
	return len(g) == 1 && g[0] >= 'a' && g[0] <= 'e'
}
