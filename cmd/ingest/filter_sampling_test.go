package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"testing"
)

// filtSampler builds filters that only sample (no country filter, nothing
// required).
func filtSampler(k int) filters {
	return newFilters(config{SampleEvery: k})
}

// sampleLit is one code with its independently computed hashes and outcomes.
// The values come from a separate Python program (FNV-1a 64 and the MurmurHash3
// fmix64 finalizer written from the definitions), not from this package.
type sampleLit struct {
	code string
	fnv  uint64 // fnv1a64(code)
	mix  uint64 // mix64(fnv1a64(code)), the value the sampler takes modulo k
	k8   bool   // kept with --sample-every 8
	k3   bool   // kept with --sample-every 3
	k2   bool   // kept with --sample-every 2
}

var sampleLits = []sampleLit{
	{"", 0xcbf29ce484222325, 0xefd01f60ba992926, false, false, true},
	{"a", 0xaf63dc4c8601ec8c, 0x82a2a958a9bece5b, false, false, false},
	{"3017620422003", 0x7a2f55318259602f, 0xb678a8657aeb8df0, true, false, true},
	{"5449000000996", 0x835ed5219312b931, 0xd4a94bb38a774ecf, false, false, false},
	{"4006381333931", 0xdc8d6baff65dd5bd, 0x81e4e41b3ef39798, true, false, true},
	{"012345678905", 0x734f049e42c9ea89, 0xb691e8533e608970, true, false, true},
	{"96385074", 0x589966b595b49403, 0xec7d8b5fa2c1b34f, false, false, false},
	{"0000000042", 0x87d61e0553bb1c43, 0xf25af7449546bf72, false, false, true},
	{"ABC-123", 0x0359ef08b37d8e86, 0x4a264cbea0fa6d9e, false, false, true},
	{"code-3", 0x6ce2654152fe8e38, 0xdff34047613cb50a, false, false, true},
	{"code-16", 0x03471c0006918078, 0x1f2529b472124ab2, false, false, true},
	{"code-2", 0x6ce2664152fe8feb, 0x5207d5dabc6ac16b, false, false, false},
	{"0016000502642", 0x0e2c442f6e657a25, 0x2b600b5b94d6e394, false, true, true},
	{"9990000000001", 0xe842f840c0a80acd, 0xb2c7b21da900d917, false, true, false},
	{"0046872021545", 0x8bda49c8d548c737, 0xb309a1ef6735f0d1, false, false, false},
	{"7622210449283", 0x2ef5b05a174736b5, 0xecd8bf4c81a2d12b, false, false, false},
	{"8076800195057", 0xf7b610cb47066ab7, 0x1743e35434063f62, false, false, true},
	{"0012000161155", 0x264d2165b03627cd, 0x1c563c207e25c7b0, true, true, true},
}

// The hash is pinned by literals: changing it would silently change which
// products an existing index holds after the next nightly run.
func TestFnv1a64_Literals(t *testing.T) {
	for _, tt := range sampleLits {
		if got := fnv1a64(tt.code); got != tt.fnv {
			t.Errorf("fnv1a64(%q) = %#x, want %#x", tt.code, got, tt.fnv)
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(tt.code))
		if got := h.Sum64(); got != tt.fnv {
			t.Errorf("hash/fnv New64a(%q) = %#x, want %#x (literal is wrong)", tt.code, got, tt.fnv)
		}
	}
}

// mix64 is MurmurHash3's fmix64: pinned on raw inputs (0 is a fixed point, the
// others exercise every shift and both multipliers) and on the real hashes.
func TestMix64_Literals(t *testing.T) {
	tests := []struct{ in, want uint64 }{
		{0, 0},
		{1, 0xb456bcfc34c2cb2c},
		{2, 0x3abf2a20650683e7},
		{1 << 63, 0x8f780810af31a493},
		{math.MaxUint64, 0x64b5720b4b825f21},
	}
	for _, tt := range tests {
		if got := mix64(tt.in); got != tt.want {
			t.Errorf("mix64(%#x) = %#x, want %#x", tt.in, got, tt.want)
		}
	}
	for _, tt := range sampleLits {
		if got := mix64(tt.fnv); got != tt.mix {
			t.Errorf("mix64(fnv1a64(%q)) = %#x, want %#x", tt.code, got, tt.mix)
		}
	}
}

// The keep/skip outcome per k, from the literal mixed hashes (value mod k == 0).
func TestFilters_Sampled_Literals(t *testing.T) {
	for _, tt := range sampleLits {
		for _, c := range []struct {
			k    int
			want bool
		}{{8, tt.k8}, {3, tt.k3}, {2, tt.k2}, {1, true}} {
			if got := filtSampler(c.k).sampled(tt.code); got != c.want {
				t.Errorf("k=%d sampled(%q) = %v, want %v", c.k, tt.code, got, c.want)
			}
		}
		// The table itself must agree with the pinned mixed hash.
		if (tt.mix%8 == 0) != tt.k8 || (tt.mix%3 == 0) != tt.k3 || (tt.mix%2 == 0) != tt.k2 {
			t.Errorf("literal outcomes for %q disagree with mix %#x", tt.code, tt.mix)
		}
	}
}

func TestFilters_Sampled_Deterministic(t *testing.T) {
	f := filtSampler(8)
	for i := 0; i < 2000; i++ {
		code := fmt.Sprintf("%013d", i*7919)
		first := f.sampled(code)
		for j := 0; j < 3; j++ {
			if f.sampled(code) != first {
				t.Fatalf("sampled(%q) changed between calls", code)
			}
		}
		if again := filtSampler(8).sampled(code); again != first {
			t.Fatalf("sampled(%q) differs between two filters", code)
		}
	}
}

// A decision depends on the code alone: the same code gets the same answer
// whatever was asked before, in any order, from any filters value.
func TestFilters_Sampled_IndependentOfOtherRows(t *testing.T) {
	codes := make([]string, 3000)
	for i := range codes {
		codes[i] = fmt.Sprintf("%013d", i*104729)
	}
	f := filtSampler(8)
	want := make(map[string]bool, len(codes))
	for _, c := range codes {
		want[c] = f.sampled(c)
	}
	r := rand.New(rand.NewSource(7))
	r.Shuffle(len(codes), func(i, j int) { codes[i], codes[j] = codes[j], codes[i] })
	for _, c := range codes {
		if got := filtSampler(8).sampled(c); got != want[c] {
			t.Fatalf("sampled(%q) = %v after shuffling, want %v", c, got, want[c])
		}
	}
}

func TestFilters_Sampled_KOneKeepsEverything(t *testing.T) {
	f := filtSampler(1)
	for i := 0; i < 5000; i++ {
		if !f.sampled(fmt.Sprintf("%d", i)) {
			t.Fatalf("k=1 skipped code %d", i)
		}
	}
}

// A zero-value filters (sampleEvery 0) must not divide by zero.
func TestFilters_Sampled_ZeroValueKeepsEverything(t *testing.T) {
	var f filters
	if !f.sampled("anything") {
		t.Error("zero-value filters skipped a code")
	}
	if got, _ := f.check(record{code: "x"}); got != skipNone {
		t.Errorf("zero-value filters check = %v, want none", got)
	}
}
