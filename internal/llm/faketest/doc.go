// Package faketest is a replay llm.Provider for tests. It serves recorded
// responses from a script, in order, and checks each request against what
// the script expects: the model, the conversation length, and that every
// tool call in the previous response is answered by a tool result in this
// request. A test that drifts from its recording fails loudly instead of
// receiving a reply meant for a different conversation.
//
// No test calls a real provider; this package is how they avoid it.
package faketest
