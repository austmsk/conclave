package llm

import "github.com/austmsk/conclave/internal/contracts"

// bytesPerToken is a deliberately low ratio: English prose averages about
// four bytes a token and source code fewer, so dividing by three
// overestimates for most content. Overestimating reserves too much, which
// is the safe direction for a budget.
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
