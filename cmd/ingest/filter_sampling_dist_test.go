package main

import (
	"fmt"
	"math/rand"
	"testing"
)

// Regression for the EAN check-digit bias: a valid EAN-13 code has an even
// digit sum, so its FNV-1a hash is always odd and "hash % k" with an even k
// kept almost nothing on the real export. The sampler must keep about 1 row in
// k for every k and every kind of code the export holds, so these tests use
// codes with VALID check digits (sequential digit strings are not enough).

const distN = 80000

// gs1Check returns the GS1 check digit of the digits in body (weights 3 and 1
// from the right, so the digit next to the check digit weighs 3).
func gs1Check(body string) byte {
	sum := 0
	for i := len(body) - 1; i >= 0; i-- {
		w := 3
		if (len(body)-1-i)%2 == 1 {
			w = 1
		}
		sum += int(body[i]-'0') * w
	}
	return byte('0' + (10-sum%10)%10)
}

// withCheck appends the check digit to body.
func withCheck(body string) string { return body + string(gs1Check(body)) }

func TestGS1Check_KnownCodes(t *testing.T) {
	for _, code := range []string{"3017620422003", "5449000000996", "4006381333931", "012345678905", "96385074"} {
		if got := withCheck(code[:len(code)-1]); got != code {
			t.Errorf("withCheck(%q) = %q, want %q", code[:len(code)-1], got, code)
		}
	}
}

// distPopulation builds n distinct codes: gen(i, r) makes the i-th one.
func distPopulation(n int, gen func(i int, r *rand.Rand) string) []string {
	r := rand.New(rand.NewSource(20261111))
	seen := make(map[string]struct{}, n)
	codes := make([]string, 0, n)
	for i := 0; len(codes) < n; i++ {
		c := gen(i, r)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		codes = append(codes, c)
	}
	return codes
}

// distKeptRatio returns kept / (n / k) for the codes.
func distKeptRatio(codes []string, k int) (kept int, ratio float64) {
	f := filtSampler(k)
	for _, c := range codes {
		if f.sampled(c) {
			kept++
		}
	}
	return kept, float64(kept) * float64(k) / float64(len(codes))
}

func TestFilters_Sampled_AboutOneInK_Populations(t *testing.T) {
	prefixes := []string{"30", "50", "40", "76", "80", "00", "37", "978"}
	pops := []struct {
		name string
		gen  func(i int, r *rand.Rand) string
	}{
		{"EAN-13 sequential bodies", func(i int, _ *rand.Rand) string {
			p := prefixes[i%len(prefixes)]
			return withCheck(p + fmt.Sprintf("%0*d", 12-len(p), i/len(prefixes)))
		}},
		{"EAN-13 random bodies", func(_ int, r *rand.Rand) string {
			p := prefixes[r.Intn(len(prefixes))]
			return withCheck(p + fmt.Sprintf("%0*d", 12-len(p), r.Int63n(pow10(12-len(p)))))
		}},
		{"UPC-A random bodies", func(_ int, r *rand.Rand) string {
			return withCheck(fmt.Sprintf("%011d", r.Int63n(pow10(11))))
		}},
		{"UPC-A sequential bodies", func(i int, _ *rand.Rand) string {
			return withCheck(fmt.Sprintf("0%010d", 1000000+i))
		}},
		{"EAN-8 codes", func(_ int, r *rand.Rand) string {
			return withCheck(fmt.Sprintf("%07d", r.Int63n(pow10(7))))
		}},
		{"short numeric with leading zeros", func(i int, _ *rand.Rand) string {
			return fmt.Sprintf("%0*d", 6+i%5, i)
		}},
		{"alphanumeric internal codes", func(i int, r *rand.Rand) string {
			return fmt.Sprintf("SKU-%c%c-%d", 'A'+r.Intn(26), 'A'+r.Intn(26), i)
		}},
	}
	for _, pop := range pops {
		t.Run(pop.name, func(t *testing.T) {
			if len(pop.name) == 0 {
				t.Fatal("unnamed population")
			}
			codes := distPopulation(distN, pop.gen)
			for _, k := range []int{2, 3, 4, 5, 6, 8, 16} {
				kept, ratio := distKeptRatio(codes, k)
				if ratio < 0.95 || ratio > 1.05 {
					t.Errorf("k=%d kept %d of %d, want %d +-5%%", k, kept, distN, distN/k)
				}
			}
		})
	}
}

func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// A population shaped like the export (mostly EAN-13, some UPC-A, EAN-8, short
// and internal codes) keeps about 1 row in k at the default k.
func TestFilters_Sampled_DefaultK_FixtureLikeMix(t *testing.T) {
	codes := distPopulation(distN, func(_ int, r *rand.Rand) string {
		switch x := r.Intn(100); {
		case x < 70:
			return withCheck(fmt.Sprintf("%012d", r.Int63n(pow10(12))))
		case x < 85:
			return withCheck(fmt.Sprintf("%011d", r.Int63n(pow10(11))))
		case x < 92:
			return withCheck(fmt.Sprintf("%07d", r.Int63n(pow10(7))))
		case x < 97:
			return fmt.Sprintf("%0*d", 4+r.Intn(6), r.Int63n(100000))
		default:
			return fmt.Sprintf("INT%d", r.Int63())
		}
	})
	kept, ratio := distKeptRatio(codes, defaultSampleEvery)
	if ratio < 0.95 || ratio > 1.05 {
		t.Errorf("k=%d kept %d of %d, want %d +-5%%", defaultSampleEvery, kept, len(codes), len(codes)/defaultSampleEvery)
	}
}

// The ids kept at one k are a stable subset of the same population every run
// (the nightly sample must not drift).
func TestFilters_Sampled_StableAcrossRuns(t *testing.T) {
	codes := distPopulation(5000, func(_ int, r *rand.Rand) string {
		return withCheck(fmt.Sprintf("%012d", r.Int63n(pow10(12))))
	})
	f := filtSampler(8)
	first := make([]bool, len(codes))
	for i, c := range codes {
		first[i] = f.sampled(c)
	}
	for i, c := range codes {
		if filtSampler(8).sampled(c) != first[i] {
			t.Fatalf("sampled(%q) changed between runs", c)
		}
	}
}

// Avalanche on the bits the modulus reads: flipping any one input bit of mix64
// must flip each of the low output bits (the ones "% k" uses for k = 2, 4, 8,
// 16) about half of the time. A finalizer missing one of its shifts keeps
// whole input bits away from the low output bits and fails this by a wide
// margin (flip rates near 0 or 1); the correct one stays within about 0.06.
func TestMix64_Avalanche_LowBits(t *testing.T) {
	const (
		inputs  = 400
		lowBits = 4
		maxDev  = 0.15
	)
	r := rand.New(rand.NewSource(1))
	xs := make([]uint64, inputs)
	for i := range xs {
		xs[i] = r.Uint64()
	}
	worst := 0.0
	for in := 0; in < 64; in++ {
		var flips [lowBits]int
		for _, x := range xs {
			d := mix64(x) ^ mix64(x^(1<<in))
			for out := 0; out < lowBits; out++ {
				flips[out] += int(d >> out & 1)
			}
		}
		for out, n := range flips {
			dev := float64(n)/inputs - 0.5
			if dev < 0 {
				dev = -dev
			}
			if dev > worst {
				worst = dev
			}
			if dev > maxDev {
				t.Errorf("input bit %d flips output bit %d in %d of %d cases (|p-0.5| = %.3f, want <= %.2f)",
					in, out, n, inputs, dev, maxDev)
			}
		}
	}
	t.Logf("worst |p-0.5| = %.3f", worst)
}
