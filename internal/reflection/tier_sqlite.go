package reflection

import (
	"context"
	"strings"
	"unicode"
)

// SQLiteConsolidator performs mechanical deduplication using Jaccard similarity.
// Lowest tier: always available, zero external dependencies, but only merges
// near-duplicates without summarizing or restructuring.
type SQLiteConsolidator struct{}

// NewSQLiteConsolidator creates a consolidator that deduplicates via token overlap.
func NewSQLiteConsolidator() *SQLiteConsolidator {
	return &SQLiteConsolidator{}
}

func (s *SQLiteConsolidator) Name() string { return "sqlite" }

func (s *SQLiteConsolidator) Available(_ context.Context) bool { return true }

func (s *SQLiteConsolidator) Consolidate(_ context.Context, input ReflectionInput) (ReflectionResult, error) {
	mems := input.ExistingMemories
	if len(mems) == 0 {
		return ReflectionResult{LearnedContext: input.CurrentContext}, nil
	}

	// Build token sets for each memory.
	type tokenized struct {
		tokens map[string]bool
	}

	items := make([]tokenized, len(mems))
	for i, m := range mems {
		items[i] = tokenized{tokens: tokenize(m.Content)}
	}

	// Find and merge duplicates (Jaccard >= 0.5, same category only).
	absorbed := make([]bool, len(items))
	var result []ReflectMemory

	for i := range items {
		if absorbed[i] {
			continue
		}

		best := mems[i]
		for j := i + 1; j < len(items); j++ {
			if absorbed[j] {
				continue
			}
			if mems[i].Category != mems[j].Category {
				continue
			}

			sim := jaccard(items[i].tokens, items[j].tokens)
			if c := containment(items[i].tokens, items[j].tokens); c > sim {
				sim = c
			}
			if sim >= 0.5 && !numericConflict(items[i].tokens, items[j].tokens) {
				absorbed[j] = true
				if mems[j].Importance > best.Importance {
					best.Importance = mems[j].Importance
				}
				if len(mems[j].Content) > len(best.Content) {
					best.Content = mems[j].Content
				}
				// Union tags.
				tagSet := make(map[string]bool)
				for _, t := range best.Tags {
					tagSet[t] = true
				}
				for _, t := range mems[j].Tags {
					tagSet[t] = true
				}
				best.Tags = make([]string, 0, len(tagSet))
				for t := range tagSet {
					best.Tags = append(best.Tags, t)
				}
			}
		}

		result = append(result, ReflectMemory{
			Category:   best.Category,
			Content:    best.Content,
			Importance: best.Importance,
			Tags:       best.Tags,
			Scope:      inferGlobalScope(best.Category, best.Content),
		})
	}

	return ReflectionResult{
		LearnedContext: input.CurrentContext,
		Memories:       result,
	}, nil
}

// stopwords are filler words that carry no consolidation signal; dropping them
// keeps Jaccard/containment from being diluted on longer memories.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

// tokenize splits text into a set of lowercase word tokens (length > 1),
// excluding stopwords.
func tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) > 1 && !stopwords[word] {
			tokens[word] = true
		}
	}
	return tokens
}

// inferGlobalScope uses keyword heuristics to detect memories that apply across
// all repositories rather than being project-specific. Used by the SQLite tier
// which cannot use LLM classification.
// Secret-looking content is never promoted to global scope: the LLM tier's
// extraction prompt explicitly excludes secrets, and global memories are
// replayed into every project's injected context, so promoting a credential
// here would widen its blast radius instead of containing it.
func inferGlobalScope(category, content string) string {
	lower := strings.ToLower(content)

	if looksLikeSecret(lower) {
		return "project"
	}

	// Preferences and certain facts are strong global signals.
	if category == "preference" {
		return "global"
	}

	// Cross-repo workflow indicators.
	globalPatterns := []string{
		"across all", "all repos", "all projects", "every repo", "every project",
		"cross-repo", "cross-project", "from any repo",
		"deploy to", "deploy from", "push to infra",
		"ssh ", "ssh into", "hostname",
		"always use", "never use", "prefer ",
		"personal tool", "dev machine", "workstation",
		"infrastructure topology", "cluster ",
	}
	for _, p := range globalPatterns {
		if strings.Contains(lower, p) {
			return "global"
		}
	}

	return "project"
}

// looksLikeSecret flags content that plausibly contains a credential.
// lower must already be lowercased.
func looksLikeSecret(lower string) bool {
	secretPatterns := []string{
		"api key", "api_key", "apikey", "credential", "password",
		"secret ", "secret_", "token ", "token_", "access_token",
		"private key", "bearer ",
	}
	for _, p := range secretPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// containment is the overlap coefficient |A∩B| / min(|A|,|B|): it catches a
// memory that is a strict subset of another ("use sqlite" inside "use sqlite
// for storage"), which symmetric Jaccard scores too low to merge.
func containment(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	denom := len(a)
	if len(b) < denom {
		denom = len(b)
	}
	return float64(intersection) / float64(denom)
}

// numericConflict reports whether the two token sets differ on a purely-numeric
// token — a precise fact ("port 80" vs "port 81") that must not be merged even
// when the surrounding words overlap heavily.
func numericConflict(a, b map[string]bool) bool {
	for token := range a {
		if isNumericToken(token) && !b[token] {
			return true
		}
	}
	for token := range b {
		if isNumericToken(token) && !a[token] {
			return true
		}
	}
	return false
}

func isNumericToken(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

// jaccard computes the Jaccard similarity coefficient between two token sets.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}

	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
