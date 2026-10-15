// Package anthropic is the llm.Provider adapter for Anthropic's Messages
// API, written on net/http. It translates normalized messages, tool
// schemas, tool calls, and tool results to and from the wire format, and
// maps HTTP failures onto the llm package's typed errors so the router can
// decide on fallback without knowing the vendor (RL-6).
//
// The API key is held by the adapter and sent only in the x-api-key header.
// It never appears in an error, a log line, or a trace attribute.
package anthropic
