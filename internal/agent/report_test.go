package agent

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

const validReport = `{
	"summary": "Count ballots per precinct",
	"files_changed": ["tally/count.go"],
	"tests_added": ["TestCountPerPrecinct"],
	"decisions": [{
		"subject": "map or slice",
		"options_considered": ["map", "slice"],
		"chosen": "map",
		"rationale": "precinct IDs are sparse"
	}],
	"uncertainties": [],
	"dependencies_requested": ["golang.org/x/text"]
}`

// reportWith returns validReport with one top-level field replaced, or
// removed when v is nil.
func reportWith(t *testing.T, key string, v any) string {
	t.Helper()
	return reportEdit(t, func(m map[string]any) {
		if v == nil {
			delete(m, key)
			return
		}
		m[key] = v
	})
}

func reportEdit(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(validReport), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// decisionWith edits the first decision.
func decisionWith(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	return reportEdit(t, func(m map[string]any) {
		edit(m["decisions"].([]any)[0].(map[string]any))
	})
}

type jsonNull struct{}

func (jsonNull) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

type reportCase struct {
	name string
	raw  string
	// want is a substring of the error, naming the offending path; empty
	// means the report is valid.
	want string
}

// reportFieldCases cover each field being present, missing, or unknown.
func reportFieldCases(t *testing.T) []reportCase {
	t.Helper()
	return []reportCase{
		{name: "valid", raw: validReport},
		{name: "empty arrays are fine", raw: reportEdit(t, func(m map[string]any) {
			for _, k := range []string{"files_changed", "tests_added", "decisions", "uncertainties"} {
				m[k] = []any{}
			}
		})},
		{name: "dependencies_requested is optional", raw: reportWith(t, "dependencies_requested", nil)},
		{name: "missing summary", raw: reportWith(t, "summary", nil), want: "report.summary is required"},
		{name: "missing files_changed", raw: reportWith(t, "files_changed", nil), want: "report.files_changed is required"},
		{name: "missing tests_added", raw: reportWith(t, "tests_added", nil), want: "report.tests_added is required"},
		{name: "missing decisions", raw: reportWith(t, "decisions", nil), want: "report.decisions is required"},
		{name: "missing uncertainties", raw: reportWith(t, "uncertainties", nil), want: "report.uncertainties is required"},
		{name: "missing decision subject", raw: decisionWith(t, func(d map[string]any) { delete(d, "subject") }),
			want: "report.decisions[0].subject is required"},
		{name: "missing decision options_considered", raw: decisionWith(t, func(d map[string]any) { delete(d, "options_considered") }),
			want: "report.decisions[0].options_considered is required"},
		{name: "missing decision chosen", raw: decisionWith(t, func(d map[string]any) { delete(d, "chosen") }),
			want: "report.decisions[0].chosen is required"},
		{name: "missing decision rationale", raw: decisionWith(t, func(d map[string]any) { delete(d, "rationale") }),
			want: "report.decisions[0].rationale is required"},
		{name: "unknown top-level field", raw: reportWith(t, "confidence", "high"),
			want: `report has an unknown field "confidence"`},
		{name: "unknown nested field", raw: decisionWith(t, func(d map[string]any) { d["weight"] = 1 }),
			want: `report.decisions[0] has an unknown field "weight"`},
	}
}

// reportShapeCases cover nulls, wrong types, empty strings and framing.
func reportShapeCases(t *testing.T) []reportCase {
	t.Helper()
	return []reportCase{
		{name: "null required array", raw: reportWith(t, "uncertainties", jsonNull{}),
			want: "report.uncertainties must be an array, got null; send [] if there are none"},
		{name: "null optional array", raw: reportWith(t, "dependencies_requested", jsonNull{}),
			want: "report.dependencies_requested must be an array, got null"},
		{name: "null nested array", raw: decisionWith(t, func(d map[string]any) { d["options_considered"] = jsonNull{} }),
			want: "report.decisions[0].options_considered must be an array, got null"},
		{name: "trailing data", raw: validReport + ` {}`, want: "more data follows"},
		{name: "trailing garbage", raw: validReport + `x`, want: "more data follows"},
		{name: "two reports", raw: validReport + validReport, want: "more data follows"},
		{name: "not JSON", raw: `summary: done`, want: "not valid JSON"},
		{name: "empty arguments", raw: ``, want: "arguments are empty"},
		{name: "null arguments", raw: `null`, want: "report must be an object, got null"},
		{name: "array arguments", raw: `[]`, want: "report must be an object, got an array"},
		{name: "summary is a number", raw: reportWith(t, "summary", 42), want: "report.summary must be a string, got a number"},
		{name: "files_changed is a string", raw: reportWith(t, "files_changed", "a.go"),
			want: "report.files_changed must be an array, got a string"},
		{name: "files_changed element is a number", raw: reportWith(t, "files_changed", []any{"a.go", 7}),
			want: "report.files_changed[1] must be a string, got a number"},
		{name: "decisions is an object", raw: reportWith(t, "decisions", map[string]any{}),
			want: "report.decisions must be an array, got an object"},
		{name: "decision is a string", raw: reportWith(t, "decisions", []any{"used a map"}),
			want: "report.decisions[0] must be an object, got a string"},
		{name: "decision chosen is a boolean", raw: decisionWith(t, func(d map[string]any) { d["chosen"] = true }),
			want: "report.decisions[0].chosen must be a string, got a boolean"},
		{name: "empty summary", raw: reportWith(t, "summary", ""), want: "report.summary must not be empty"},
		{name: "blank summary", raw: reportWith(t, "summary", " \n\t"), want: "report.summary must not be empty"},
		{name: "empty rationale", raw: decisionWith(t, func(d map[string]any) { d["rationale"] = "" }),
			want: "report.decisions[0].rationale must not be empty"},
	}
}

func TestValidateReport(t *testing.T) {
	cases := append(reportFieldCases(t), reportShapeCases(t)...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := validateReport(json.RawMessage(tc.raw))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid report refused: %v", err)
				}
				if report.Summary == "" {
					t.Fatalf("report not decoded: %+v", report)
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid report accepted: %+v", report)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportDecodes(t *testing.T) {
	got, err := validateReport(json.RawMessage(validReport))
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.CompletionReport{
		Summary:      "Count ballots per precinct",
		FilesChanged: []string{"tally/count.go"},
		TestsAdded:   []string{"TestCountPerPrecinct"},
		Decisions: []contracts.Decision{{
			Subject: "map or slice", OptionsConsidered: []string{"map", "slice"},
			Chosen: "map", Rationale: "precinct IDs are sparse",
		}},
		Uncertainties:         []string{},
		DependenciesRequested: []string{"golang.org/x/text"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestValidateReportListsEveryProblem(t *testing.T) {
	raw := reportEdit(t, func(m map[string]any) {
		delete(m, "summary")
		m["files_changed"] = jsonNull{}
		m["extra"] = 1
	})
	_, err := validateReport(json.RawMessage(raw))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{`unknown field "extra"`, "report.summary is required", "report.files_changed must be an array"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// schemaObject is the subset of JSON Schema finishSchema emits for an
// object.
type schemaObject struct {
	Type                 string                     `json:"type"`
	Description          string                     `json:"description"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
}

// TestFinishSchemaMatchesValidator checks the schema the model is shown
// against the contract and against what the validator accepts: every
// property is a json field of the contract type, the required list is the
// fields without omitempty, and the validator accepts exactly those.
func TestFinishSchemaMatchesValidator(t *testing.T) {
	var top schemaObject
	if err := json.Unmarshal(finishSchema(), &top); err != nil {
		t.Fatalf("finishSchema is not valid JSON: %v", err)
	}
	if top.Description == "" {
		t.Error("schema has no description")
	}
	checkSchemaObject(t, "report", top, reflect.TypeFor[contracts.CompletionReport]())

	var decisions struct {
		Items schemaObject `json:"items"`
	}
	if err := json.Unmarshal(top.Properties["decisions"], &decisions); err != nil {
		t.Fatal(err)
	}
	checkSchemaObject(t, "decision", decisions.Items, reflect.TypeFor[contracts.Decision]())

	for _, name := range slices.Sorted(maps.Keys(top.Properties)) {
		raw := reportWith(t, name, nil)
		_, err := validateReport(json.RawMessage(raw))
		required := slices.Contains(top.Required, name)
		if required != (err != nil) {
			t.Errorf("removing %s: schema required=%v, validator error=%v", name, required, err)
		}
	}
	if _, err := validateReport(json.RawMessage(reportWith(t, "not_in_schema", "x"))); err == nil {
		t.Error("validator accepts a field the schema does not list")
	}
}

func checkSchemaObject(t *testing.T, what string, s schemaObject, typ reflect.Type) {
	t.Helper()
	if s.Type != "object" || s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Errorf("%s: want a closed object, got type %q additionalProperties %v", what, s.Type, s.AdditionalProperties)
	}
	var wantProps, wantRequired []string
	for f := range typ.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		wantProps = append(wantProps, name)
		if !strings.Contains(opts, "omitempty") {
			wantRequired = append(wantRequired, name)
		}
	}
	gotProps := slices.Sorted(maps.Keys(s.Properties))
	slices.Sort(wantProps)
	if !slices.Equal(gotProps, wantProps) {
		t.Errorf("%s properties: got %v, want %v", what, gotProps, wantProps)
	}
	gotRequired := slices.Sorted(slices.Values(s.Required))
	slices.Sort(wantRequired)
	if !slices.Equal(gotRequired, wantRequired) {
		t.Errorf("%s required: got %v, want %v", what, gotRequired, wantRequired)
	}
}
