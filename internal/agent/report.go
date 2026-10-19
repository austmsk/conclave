package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// The finish report is defined once, in reportFields. finishSchema renders
// it as the JSON Schema the model is shown and validateReport enforces it,
// so the two cannot drift apart. A test checks the definition against the
// json tags of contracts.CompletionReport.

// fieldKind is the JSON shape a report field must have.
type fieldKind int

const (
	kindString fieldKind = iota
	kindStringArray
	kindObjectArray
)

// field is one property of a report object.
type field struct {
	name     string
	kind     fieldKind
	required bool
	// nonEmpty applies to strings: "" is refused.
	nonEmpty bool
	desc     string
	// items are the properties of each element of a kindObjectArray.
	items []field
}

var decisionFields = []field{
	{name: "subject", kind: kindString, required: true, nonEmpty: true, desc: "What the decision was about."},
	{name: "options_considered", kind: kindStringArray, required: true, desc: "The options weighed, including the one chosen."},
	{name: "chosen", kind: kindString, required: true, nonEmpty: true, desc: "The option taken."},
	{name: "rationale", kind: kindString, required: true, nonEmpty: true, desc: "Why it was taken over the others."},
}

var reportFields = []field{
	{name: "summary", kind: kindString, required: true, nonEmpty: true,
		desc: "What was changed and why, for the pull request description."},
	{name: "files_changed", kind: kindStringArray, required: true,
		desc: "Every file the change touches, as a workspace-relative path. Checked against the actual diff."},
	{name: "tests_added", kind: kindStringArray, required: true,
		desc: "Tests added or changed, by name or file. Use [] if none."},
	{name: "decisions", kind: kindObjectArray, required: true, items: decisionFields,
		desc: "Non-obvious choices made during the work. Use [] if none."},
	{name: "uncertainties", kind: kindStringArray, required: true,
		desc: "Risks and anything you are unsure of, for the pull request's risk section. Use [] if none."},
	{name: "dependencies_requested", kind: kindStringArray,
		desc: "Dependencies the change needs that are not already in the repository. Omit if none."},
}

const finishDescription = "Ends the session with a completion report. Call it once, alone, " +
	"when the work is done. Every field except dependencies_requested is required; " +
	"lists with nothing in them are sent as [], never null."

// finishSchema is the JSON Schema of the finish tool's arguments, built
// from the same definition validateReport enforces.
func finishSchema() json.RawMessage {
	s := objectSchema(reportFields)
	s["description"] = finishDescription
	return marshalSchema(s)
}

func objectSchema(fields []field) map[string]any {
	props := make(map[string]any, len(fields))
	required := []string{}
	for _, f := range fields {
		props[f.name] = fieldSchema(f)
		if f.required {
			required = append(required, f.name)
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func fieldSchema(f field) map[string]any {
	var s map[string]any
	switch f.kind {
	case kindString:
		s = map[string]any{"type": "string"}
		if f.nonEmpty {
			s["minLength"] = 1
		}
	case kindStringArray:
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case kindObjectArray:
		s = map[string]any{"type": "array", "items": objectSchema(f.items)}
	}
	s["description"] = f.desc
	return s
}

// maxReportProblems bounds how many problems one error lists, so a report
// with hundreds of stray fields does not flood the model's context.
const maxReportProblems = 20

// validateReport decodes and validates a finish report. Its error message
// is written for the model, which gets exactly one chance to repair, so it
// lists every problem found, each naming the field's path.
//
// The arguments are decoded into a map first, so a missing field is told
// apart from a zero one, and only then into the typed report.
func validateReport(raw json.RawMessage) (contracts.CompletionReport, error) {
	v, err := decodeJSONValue(raw)
	if err != nil {
		return contracts.CompletionReport{}, fmt.Errorf("invalid finish report: %w", err)
	}
	var problems []string
	checkObject("report", v, reportFields, &problems)
	if len(problems) > 0 {
		return contracts.CompletionReport{}, reportError(problems)
	}
	var report contracts.CompletionReport
	if err := json.Unmarshal(raw, &report); err != nil {
		// The shape was checked above, so this is a bug, not bad input.
		return contracts.CompletionReport{}, fmt.Errorf("decoding validated finish report: %w", err)
	}
	return report, nil
}

// decodeJSONValue decodes exactly one JSON value, refusing trailing data.
func decodeJSONValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var v any
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the arguments are empty; send the report as one JSON object")
		}
		return nil, fmt.Errorf("the arguments are not valid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the arguments must be exactly one JSON object, but more data follows it")
	}
	return v, nil
}

func reportError(problems []string) error {
	extra := 0
	if len(problems) > maxReportProblems {
		extra = len(problems) - maxReportProblems
		problems = problems[:maxReportProblems]
	}
	msg := "invalid finish report; fix every problem below and call finish again:\n- " + strings.Join(problems, "\n- ")
	if extra > 0 {
		msg += fmt.Sprintf("\n- and %d more", extra)
	}
	return errors.New(msg)
}

// checkObject appends a problem for each way v fails to be an object with
// exactly fields. Unknown keys are reported in sorted order, then fields
// in definition order, so the message is stable.
func checkObject(path string, v any, fields []field, problems *[]string) {
	obj, ok := v.(map[string]any)
	if !ok {
		*problems = append(*problems, fmt.Sprintf("%s must be an object, got %s", path, jsonType(v)))
		return
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !slices.ContainsFunc(fields, func(f field) bool { return f.name == k }) {
			*problems = append(*problems, fmt.Sprintf("%s has an unknown field %q; the allowed fields are %s",
				path, k, fieldNames(fields)))
		}
	}
	for _, f := range fields {
		val, present := obj[f.name]
		switch {
		case !present && f.required:
			*problems = append(*problems, fmt.Sprintf("%s.%s is required", path, f.name))
		case present:
			checkField(path+"."+f.name, val, f, problems)
		}
	}
}

func checkField(path string, v any, f field, problems *[]string) {
	if f.kind == kindString {
		checkString(path, v, f.nonEmpty, problems)
		return
	}
	arr, ok := v.([]any)
	if !ok {
		hint := ""
		if v == nil {
			hint = "; send [] if there are none"
		}
		*problems = append(*problems, fmt.Sprintf("%s must be an array, got %s%s", path, jsonType(v), hint))
		return
	}
	for i, el := range arr {
		elPath := fmt.Sprintf("%s[%d]", path, i)
		if f.kind == kindObjectArray {
			checkObject(elPath, el, f.items, problems)
			continue
		}
		checkString(elPath, el, false, problems)
	}
}

func checkString(path string, v any, nonEmpty bool, problems *[]string) {
	s, ok := v.(string)
	switch {
	case !ok:
		*problems = append(*problems, fmt.Sprintf("%s must be a string, got %s", path, jsonType(v)))
	case nonEmpty && strings.TrimSpace(s) == "":
		*problems = append(*problems, fmt.Sprintf("%s must not be empty", path))
	}
}

// jsonType names the JSON type of a value decoded into any.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func fieldNames(fields []field) string {
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.name
	}
	return strings.Join(names, ", ")
}
