package main

import "testing"

// filtCfg is the default filtering configuration, sampling off (k = 1).
func filtCfg() config {
	return config{
		Countries:   []string{"en:united-states"},
		Require:     []string{"brand", "category", "nutriscore"},
		SampleEvery: 1,
	}
}

// filtGood passes every default filter.
func filtGood() record {
	return record{
		code: "1", name: "n",
		brands: []string{"b"}, categories: []string{"c"},
		nutriscore: "a", countries: []string{"en:united-states"},
	}
}

func TestSkipReason_StringKeys(t *testing.T) {
	want := map[skipReason]string{
		skipNone:        "none",
		skipTooLong:     "too_long",
		skipMalformed:   "malformed",
		skipInvalidCode: "invalid_code",
		skipOversized:   "oversized",
		skipNoName:      "no_name",
		skipNotSampled:  "not_sampled",
		skipCountry:     "country",
		skipBrand:       "brand",
		skipCategory:    "category",
		skipNutriscore:  "nutriscore",
	}
	if len(want) != int(numSkipReasons) {
		t.Fatalf("test lists %d reasons, enum has %d", len(want), numSkipReasons)
	}
	for r, s := range want {
		if got := r.String(); got != s {
			t.Errorf("skipReason(%d).String() = %q, want %q", r, got, s)
		}
	}
}

func TestFilters_Check_EachBucket(t *testing.T) {
	tests := []struct {
		name string
		mut  func(r *record)
		want skipReason
	}{
		{"passes", func(r *record) {}, skipNone},
		{"other country", func(r *record) { r.countries = []string{"en:france"} }, skipCountry},
		{"no country", func(r *record) { r.countries = nil }, skipCountry},
		{"no brand", func(r *record) { r.brands = nil }, skipBrand},
		{"no category", func(r *record) { r.categories = nil }, skipCategory},
		{"no nutriscore", func(r *record) { r.nutriscore = "" }, skipNutriscore},
	}
	f := newFilters(filtCfg())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := filtGood()
			tt.mut(&rec)
			if got, _ := f.check(rec); got != tt.want {
				t.Errorf("check = %v, want %v", got, tt.want)
			}
		})
	}
}

// The first failing filter owns the row, in the order country, brand,
// category, nutriscore; every adjacent pair is pinned.
func TestFilters_Check_Precedence(t *testing.T) {
	noCountry := func(r *record) { r.countries = []string{"en:canada"} }
	noBrand := func(r *record) { r.brands = nil }
	noCat := func(r *record) { r.categories = nil }
	noNutri := func(r *record) { r.nutriscore = "" }
	tests := []struct {
		name string
		muts []func(*record)
		want skipReason
	}{
		{"country and brand", []func(*record){noBrand, noCountry}, skipCountry},
		{"brand and category", []func(*record){noCat, noBrand}, skipBrand},
		{"category and nutriscore", []func(*record){noNutri, noCat}, skipCategory},
		{"country and nutriscore", []func(*record){noNutri, noCountry}, skipCountry},
		{"brand and nutriscore", []func(*record){noNutri, noBrand}, skipBrand},
		{"country and category", []func(*record){noCat, noCountry}, skipCountry},
		{"all four", []func(*record){noNutri, noCat, noBrand, noCountry}, skipCountry},
	}
	f := newFilters(filtCfg())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := filtGood()
			for _, m := range tt.muts {
				m(&rec)
			}
			if got, _ := f.check(rec); got != tt.want {
				t.Errorf("check = %v, want %v", got, tt.want)
			}
		})
	}
}

// not_sampled comes before every filter.
func TestFilters_Check_NotSampledBeforeFilters(t *testing.T) {
	cfg := filtCfg()
	cfg.SampleEvery = 8
	f := newFilters(cfg)
	rec := filtGood()
	rec.code = "code-1" // mix64(fnv1a64) mod 8 != 0: not sampled
	rec.countries = []string{"en:france"}
	rec.brands, rec.categories, rec.nutriscore = nil, nil, ""
	got, pass := f.check(rec)
	if got != skipNotSampled {
		t.Errorf("check = %v, want not_sampled", got)
	}
	if pass != [numFilters]bool{} {
		t.Errorf("pass flags of an unsampled row = %v, want all false", pass)
	}
	rec.code = "3017620422003" // mix mod 8 = 0: sampled, so the country filter speaks
	if got, _ := f.check(rec); got != skipCountry {
		t.Errorf("sampled check = %v, want country", got)
	}
}

func TestFilters_Check_DisabledFilters(t *testing.T) {
	bare := record{code: "1", name: "n"} // no brand, category, grade or country
	tests := []struct {
		name string
		cfg  config
		want skipReason
	}{
		{"all off", config{SampleEvery: 1}, skipNone},
		{"empty slices are off", config{SampleEvery: 1, Countries: []string{}, Require: []string{}}, skipNone},
		{"only country on", config{SampleEvery: 1, Countries: []string{"en:france"}}, skipCountry},
		{"only brand on", config{SampleEvery: 1, Require: []string{"brand"}}, skipBrand},
		{"only category on", config{SampleEvery: 1, Require: []string{"category"}}, skipCategory},
		{"only nutriscore on", config{SampleEvery: 1, Require: []string{"nutriscore"}}, skipNutriscore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := newFilters(tt.cfg).check(bare); got != tt.want {
				t.Errorf("check = %v, want %v", got, tt.want)
			}
		})
	}
}

// Each filter being off lets its own failure through while the others still
// apply.
func TestFilters_Check_OneFilterOffOthersOn(t *testing.T) {
	all := filtCfg()
	tests := []struct {
		name string
		drop string // what to turn off
		mut  func(r *record)
	}{
		{"country off", "country", func(r *record) { r.countries = []string{"en:france"} }},
		{"brand off", "brand", func(r *record) { r.brands = nil }},
		{"category off", "category", func(r *record) { r.categories = nil }},
		{"nutriscore off", "nutriscore", func(r *record) { r.nutriscore = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := all
			cfg.Countries = append([]string(nil), all.Countries...)
			cfg.Require = nil
			if tt.drop == "country" {
				cfg.Countries = nil
				cfg.Require = all.Require
			} else {
				for _, r := range all.Require {
					if r != tt.drop {
						cfg.Require = append(cfg.Require, r)
					}
				}
			}
			rec := filtGood()
			tt.mut(&rec)
			if got, _ := newFilters(cfg).check(rec); got != skipNone {
				t.Errorf("check with %s = %v, want none", tt.name, got)
			}
		})
	}
}

func TestFilters_Check_AnyCountrySemantics(t *testing.T) {
	cfg := filtCfg()
	cfg.Countries = []string{"en:canada"}
	f := newFilters(cfg)
	tests := []struct {
		name      string
		countries []string
		want      skipReason
	}{
		{"one of two", []string{"en:france", "en:canada"}, skipNone},
		{"other order", []string{"en:canada", "en:france"}, skipNone},
		{"none of them", []string{"en:france", "en:germany"}, skipCountry},
		{"empty list", nil, skipCountry},
	}
	for _, tt := range tests {
		rec := filtGood()
		rec.countries = tt.countries
		if got, _ := f.check(rec); got != tt.want {
			t.Errorf("%s: check = %v, want %v", tt.name, got, tt.want)
		}
	}

	// A record matching ANY of several wanted countries passes (not all).
	cfg.Countries = []string{"en:canada", "en:france"}
	rec := filtGood()
	rec.countries = []string{"en:france"}
	if got, _ := newFilters(cfg).check(rec); got != skipNone {
		t.Errorf("one of two wanted countries: check = %v, want none", got)
	}
}

// The tag is compared whole: exact and case sensitive.
func TestFilters_Check_CountryExactMatch(t *testing.T) {
	f := newFilters(filtCfg()) // en:united-states
	for _, c := range []string{
		"en:united", "en:united-states-of-america", "EN:UNITED-STATES", "en:United-States",
		"united-states", "fr:united-states", " en:united-states", "en:united-states ", "",
	} {
		rec := filtGood()
		rec.countries = []string{c}
		if got, _ := f.check(rec); got != skipCountry {
			t.Errorf("country %q: check = %v, want country", c, got)
		}
	}
}

func TestFilters_Check_NutriscoreSet(t *testing.T) {
	f := newFilters(filtCfg())
	for _, g := range []string{"a", "b", "c", "d", "e"} {
		rec := filtGood()
		rec.nutriscore = g
		if got, _ := f.check(rec); got != skipNone {
			t.Errorf("grade %q: check = %v, want none", g, got)
		}
	}
	for _, g := range []string{"", "f", "z", "A", "unknown", "not-applicable", "ab", "`"} {
		rec := filtGood()
		rec.nutriscore = g
		if got, _ := f.check(rec); got != skipNutriscore {
			t.Errorf("grade %q: check = %v, want nutriscore", g, got)
		}
	}
}

func TestFilters_Check_AtLeastOneBrandAndCategory(t *testing.T) {
	f := newFilters(filtCfg())
	rec := filtGood()
	rec.brands = []string{"only"}
	rec.categories = []string{"only"}
	if got, _ := f.check(rec); got != skipNone {
		t.Errorf("one brand and one category: check = %v, want none", got)
	}
	rec.brands = []string{}
	if got, _ := f.check(rec); got != skipBrand {
		t.Errorf("empty non-nil brands: check = %v, want brand", got)
	}
}

// Independent pass flags: a row failing one filter still passes the others,
// whatever the bucket says.
func TestFilters_Check_IndependentPassFlags(t *testing.T) {
	f := newFilters(filtCfg())

	rec := filtGood()
	rec.countries = []string{"en:france"} // fails country only
	got, pass := f.check(rec)
	if got != skipCountry {
		t.Fatalf("bucket = %v, want country", got)
	}
	want := [numFilters]bool{filterCountry: false, filterBrand: true, filterCategory: true, filterNutriscore: true}
	if pass != want {
		t.Errorf("pass = %v, want %v", pass, want)
	}

	rec = filtGood()
	rec.countries = []string{"en:france"}
	rec.brands = nil
	rec.nutriscore = ""
	got, pass = f.check(rec) // fails country, brand, nutriscore
	if got != skipCountry {
		t.Fatalf("bucket = %v, want country", got)
	}
	want = [numFilters]bool{filterCountry: false, filterBrand: false, filterCategory: true, filterNutriscore: false}
	if pass != want {
		t.Errorf("pass = %v, want %v", pass, want)
	}

	got, pass = f.check(filtGood())
	if got != skipNone || pass != [numFilters]bool{true, true, true, true} {
		t.Errorf("good row: %v %v, want none and all true", got, pass)
	}
}

// A disabled filter never reports a pass, even for a row that would pass it.
func TestFilters_Check_DisabledFilterHasNoPassFlag(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
		want [numFilters]bool
	}{
		{"brand only", config{SampleEvery: 1, Require: []string{"brand"}}, [numFilters]bool{filterBrand: true}},
		{"category only", config{SampleEvery: 1, Require: []string{"category"}}, [numFilters]bool{filterCategory: true}},
		{"nutriscore only", config{SampleEvery: 1, Require: []string{"nutriscore"}}, [numFilters]bool{filterNutriscore: true}},
		{"country only", config{SampleEvery: 1, Countries: []string{"en:united-states"}}, [numFilters]bool{filterCountry: true}},
		{"none", config{SampleEvery: 1}, [numFilters]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, pass := newFilters(tt.cfg).check(filtGood())
			if pass != tt.want {
				t.Errorf("pass = %v, want %v", pass, tt.want)
			}
		})
	}
}

func TestFilters_Enabled(t *testing.T) {
	f := newFilters(filtCfg())
	for i := 0; i < numFilters; i++ {
		if !f.enabled(i) {
			t.Errorf("filter %d disabled with the default config", i)
		}
	}
	f = newFilters(config{SampleEvery: 1})
	for i := 0; i < numFilters; i++ {
		if f.enabled(i) {
			t.Errorf("filter %d enabled with everything off", i)
		}
	}
	f = newFilters(config{SampleEvery: 1, Countries: []string{"en:x"}, Require: []string{"category"}})
	want := [numFilters]bool{filterCountry: true, filterCategory: true}
	for i := 0; i < numFilters; i++ {
		if f.enabled(i) != want[i] {
			t.Errorf("enabled(%d) = %v, want %v", i, f.enabled(i), want[i])
		}
	}
}

func TestFilters_Enabled_OutOfRange(t *testing.T) {
	f := newFilters(filtCfg())
	for _, i := range []int{-1, numFilters, numFilters + 1} {
		if f.enabled(i) {
			t.Errorf("enabled(%d) = true, want false", i)
		}
	}
}
