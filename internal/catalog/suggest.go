package catalog

import "context"

// SuggestQuery is one autocomplete request. The HTTP layer validates it;
// adapters trust it.
type SuggestQuery struct {
	// Text is the typed prefix text, trimmed, 2 to 100 runes.
	Text string
	// Size is the maximum number of suggestions, 1..20.
	Size int
}

// Suggestion is one product name offered to the user.
type Suggestion struct {
	Text string
}

// Suggester returns product-name suggestions for typed text.
// An implementation returns at most q.Size suggestions, distinct after
// folding (case, accents), never nil, in rank order.
type Suggester interface {
	Suggest(ctx context.Context, q SuggestQuery) ([]Suggestion, error)
}
