package chunk

import "strings"

// WordEstimator is the default offline token counter. It approximates token
// count as ceil(words * 1.3), where words are whitespace-separated fields. The
// 1.3 factor reflects that sub-word tokenizers (BPE/wordpiece) typically split
// natural-language words into slightly more than one token on average. It needs
// no vocabulary file and is fully deterministic, satisfying the local-first
// constraint; a model-matched tokenizer replaces it at Phase 4.
type WordEstimator struct{}

// Count returns the estimated number of tokens in text.
func (WordEstimator) Count(text string) int {
	words := len(strings.Fields(text))
	if words == 0 {
		return 0
	}
	// ceil(words * 1.3) == (words*13 + 9) / 10
	return (words*13 + 9) / 10
}
