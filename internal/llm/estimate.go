package llm

import "github.com/austmsk/conclave/internal/contracts"

// bytesPerToken is a deliberately low ratio: English prose averages about
// four bytes a token, so dividing by three overestimates prose, and
// overestimating reserves too much, the safe direction for a budget. It is
// not a bound. Dense content tokenizes at fewer bytes a token than three:
// CJK text, base64, minified code, long runs of digits or punctuation. For
// those the estimate undercounts, the reservation is too small, and one
// call can take the scope past its budget on the input side before the
// real count is charged. The next call is then refused, so the per-attempt
// cap is soft by at most one call's input.
const bytesPerToken = 3

// estimateInputTokens guesses a request's input size before it is sent,
// for budget reservation and rate limiting. Providers report the real count
// afterwards, and that is what is charged.
func estimateInputTokens(req contracts.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Text)
		for _, c := range m.ToolCalls {
			n += len(c.Name) + len(c.Arguments)
		}
		for _, r := range m.ToolResults {
			n += len(r.Output) + len(r.Error)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return n/bytesPerToken + 1
}

// estimateTextTokens is estimateInputTokens for plain texts.
func estimateTextTokens(texts ...string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n/bytesPerToken + 1
}
