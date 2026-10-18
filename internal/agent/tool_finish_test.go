package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFinishTool(t *testing.T) {
	tool := newFinish()
	if string(tool.Schema()) != string(finishSchema()) {
		t.Fatal("finish tool schema is not finishSchema")
	}
	res, err := tool.Execute(t.Context(), nil, json.RawMessage(validReport))
	if err != nil || res.Output != "report accepted" {
		t.Fatalf("valid report: %+v, %v", res, err)
	}
	_, err = tool.Execute(t.Context(), nil, json.RawMessage(reportWith(t, "summary", nil)))
	var r *Refusal
	if !errors.As(err, &r) || r.Code != codeInvalidReport || r.Rejected || !strings.Contains(r.Msg, "report.summary is required") {
		t.Fatalf("invalid report: got %#v", err)
	}
}
