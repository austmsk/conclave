package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/contracts"
)

// Session limits are enforced by the runner, never asked of the model:
//   - MaxSteps is checked before each call. N allows exactly N calls; the
//     call that would be N+1 ends the session with step_limit, and it and
//     the rest of its reply are recorded as not executed.
//   - MaxTokens is checked against the input and output every reply
//     reports. Each request asks for at most what is left, and a reply
//     that takes the total past the limit ends the session with
//     token_limit before any of its calls run. The attempt's budget in the
//     router is separate and shared by every scope of the attempt; its
//     refusal is token_limit too.
//   - MaxWallClock is the session context's deadline, with errWallClock
//     as its cause.
//   - MaxRepeatErrors counts failures by repeatKey, in total rather than
//     in a row: a debugging spiral that alternates between two failures
//     still stops.

// repeatKey identifies a failure for the repeat-error limit: the tool,
// its arguments in canonical form, the exit code, and a hash of the full
// output and the error the model was shown. Two failures with one key are
// the same call failing the same way.
func repeatKey(call contracts.ToolCall, full string, result contracts.ToolResult) string {
	h := sha256.New()
	h.Write([]byte(full))
	h.Write([]byte{0})
	h.Write([]byte(result.Error))
	return string(call.Name) + "\x00" + canonicalArgs(call.Arguments) + "\x00" +
		strconv.Itoa(result.ExitCode) + "\x00" + hex.EncodeToString(h.Sum(nil))
}

// canonicalArgs re-marshals JSON arguments, which sorts object keys and
// drops insignificant space, so the same arguments sent in a different
// order or layout count as the same call. Arguments that are not JSON are
// used as they are.
func canonicalArgs(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// truncate cuts s to at most limit bytes for the model: its head and its
// tail around a marker saying how much was left out. Cuts fall on rune
// boundaries, so valid UTF-8 stays valid. The full output is in the blob
// store; the marker says so.
func truncate(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	// The marker's length is fixed by sizing its count for all of s, which
	// is at least as wide as the count it will show.
	keep := limit - len(truncationMarker(len(s)))
	if keep <= 0 {
		return s[:runeStart(s, limit)], true
	}
	headEnd := runeStart(s, keep/2)
	// The tail's cut moves forward, the head's back, so neither grows.
	tailStart := len(s) - (keep - keep/2)
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:headEnd] + truncationMarker(tailStart-headEnd) + s[tailStart:], true
}

// runeStart moves i back to the start of the rune it falls in.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func truncationMarker(omitted int) string {
	return fmt.Sprintf("\n[... %d bytes omitted; the full output is in the audit record ...]\n", omitted)
}
