// Package agenttest provides fakes for testing the agent loop without
// Docker or a real model: a Sandbox over an os.Root directory that logs
// every call, and a scoped router over the faketest replay provider.
package agenttest
