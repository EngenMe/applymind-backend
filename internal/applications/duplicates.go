package applications

import (
	"strings"
	"unicode"
)

// Deciding whether two job titles describe the same posting.
//
// The company name is already an exact match by the time anything here runs —
// FindApplicationsByCompanyName does that in SQL. What is left is the harder
// half: the same job is posted on LinkedIn as "Senior Backend Engineer (Go)" and
// on the company's own board as "Senior Backend Engineer, Go — Remote", and both
// should be recognised as one job.
//
// Pure string work on purpose, and pure Go rather than SQL: it needs no query,
// no sqlc regeneration and no database to test. Embedding-based similarity is
// explicitly a later phase.

// TitleSimilarityThreshold is how much of the meaningful wording two titles have
// to share before they are treated as the same job.
//
// 0.6 is tuned for a false positive being cheap and a false negative being
// expensive: the warning never blocks a save, so over-warning costs the user one
// glance, while under-warning is the exact mistake the product exists to
// prevent. It is deliberately above 0.5, which is where "Backend Engineer" and
// "Backend Engineer Manager" start colliding.
const TitleSimilarityThreshold = 0.6

// titleNoise is wording that appears in job titles without saying anything about
// which job it is: employment terms, work arrangements, the German
// "(m/w/d)"-style gender notes and ordinary English joining words.
//
// Seniority is deliberately absent — "Senior Go Developer" and "Junior Go
// Developer" are different jobs at the same company, and folding them together
// would warn about the wrong thing.
var titleNoise = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "of": {}, "for": {},
	"at": {}, "in": {}, "to": {}, "with": {}, "on": {},
	"remote": {}, "hybrid": {}, "onsite": {}, "office": {}, "based": {},
	"fulltime": {}, "parttime": {}, "full": {}, "part": {}, "time": {},
	"permanent": {}, "contract": {}, "temporary": {}, "freelance": {},
	"mwd": {}, "mfd": {}, "fmd": {}, "mfx": {}, "divers": {},
	"new": {}, "job": {}, "role": {}, "position": {}, "vacancy": {}, "opening": {},
}

// titleTokens reduces a title to the set of words worth comparing.
//
// Single characters are dropped along with the noise list: they are almost
// always the residue of "(m/f/d)" or a stray bullet rather than content. The
// cost is that a title distinguished only by a one-letter word — "C Developer"
// against "R Developer" — compares as identical wording; both are for the same
// company, so the warning would be worth showing anyway.
func titleTokens(title string) map[string]struct{} {
	fields := strings.FieldsFunc(
		strings.ToLower(title), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		},
	)

	tokens := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if len([]rune(field)) < 2 {
			continue
		}
		if _, noise := titleNoise[field]; noise {
			continue
		}
		tokens[field] = struct{}{}
	}
	return tokens
}

// TitlesLikelySame reports whether two job titles look like the same posting.
//
// Two ways to qualify. Containment first: one title's wording being wholly
// inside the other is the shape a cross-site duplicate actually takes, because
// company boards add detail ("Senior Backend Engineer" → "Senior Backend
// Engineer, Payments Team") rather than change it. Otherwise the overlap has to
// clear TitleSimilarityThreshold.
//
// An empty title on either side is never a match: with nothing to compare, the
// honest answer is that this is only a company match.
func TitlesLikelySame(left, right string) bool {
	a, b := titleTokens(left), titleTokens(right)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if subset(a, b) || subset(b, a) {
		return true
	}
	return jaccard(a, b) >= TitleSimilarityThreshold
}

func subset(inner, outer map[string]struct{}) bool {
	for token := range inner {
		if _, ok := outer[token]; !ok {
			return false
		}
	}
	return true
}

// jaccard is shared tokens over total distinct tokens: 1 for identical wording,
// 0 for nothing in common.
func jaccard(a, b map[string]struct{}) float64 {
	shared := 0
	for token := range a {
		if _, ok := b[token]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
