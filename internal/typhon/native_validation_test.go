package typhon

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeValidationSchemaRules(t *testing.T) {
	module := sourceFile(t, `import json
import typhon.validation as v
schema: v.Schema = v.object({
    'name': v.text(2, 2),
    'items': v.array(v.object({'quantity': v.integer(1, 3)}), 1, 3),
    'id': v.number(9007199254740993, 9007199254740993),
    'active': v.boolean()
}, {'role': v.nullable(v.enumeration(['reader', 'editor']))})
good: Any = json.loads('{"name":"é😀","items":[{"quantity":1}],"id":9007199254740993,"active":true,"role":null}')
bad: Any = json.loads('{"name":"abc","items":[{"quantity":1},{"quantity":2},{"quantity":0}],"id":9007199254740992,"active":null,"private-key":"sensitive","role":"unknown"}')
print(len(v.check(schema, good)))
for item: dict[str, str] in v.check(schema, bad):
    print(item['path'], item['rule'], item['code'])
print(len(v.check(v.object({'required':v.boolean()}, {}, True), {'private-key': 42})))
print(len(v.check(v.object({}, {}, True), {'private-key': 42})))
`)
	output, err := captureRun(t, module)
	want := "0\n$ additional_properties unexpected_field\nname max_length too_long\nitems[2].quantity minimum below_minimum\nid minimum below_minimum\nactive type invalid_type\nrole enum not_in_enum\n1\n0\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
	for _, source := range []string{
		"v.text(10, 2)",
		"v.array(v.boolean(), -1)",
		"v.integer(5, 2)",
		"v.enumeration(['same', 'same'])",
		"v.object({'x': v.text()}, {'x': v.text()})",
	} {
		output, err := captureRun(t, sourceFile(t, "import typhon.validation as v\ntry:\n    "+source+"\nexcept ValidationError:\n    print('invalid')\n"))
		if err != nil || output != "invalid\n" {
			t.Errorf("invalid schema accepted: %s, %q %v", source, output, err)
		}
	}
}

func TestNativeValidationHTTPExample(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(examplePath(t, "payload_validation.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpVMHandler(engine, module.Globals["handle"]))
	defer server.Close()
	for _, tc := range []struct {
		input, body string
		status      int
	}{
		{`{"name":"Ada","role":"reader","items":[{"quantity":2}]}`, `{"name": "Ada"}`, 201},
		{`{"name": private-secret}`, `{"error": "invalid_json"}`, 400},
		{`{"name":"Ada","role":"reader","items":[{"quantity":0}]}`,
			`{"errors": [{"path": "items[0].quantity", "rule": "minimum", "code": "below_minimum", "message": "number is below the minimum"}]}`, 422},
	} {
		response, err := server.Client().Post(server.URL, "application/json", strings.NewReader(tc.input))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != tc.status || string(body) != tc.body || !json.Valid(body) {
			t.Errorf("got %d %q (%v), want %d %q", response.StatusCode, body, err, tc.status, tc.body)
		}
	}
}

func TestNativeValidationIssueAndWorkLimits(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	vm := &VM{engine: engine}
	input := make([]Value, maxValidationSteps)
	inner := &validationSchema{kind: "boolean"}
	list := &validationSchema{kind: "array", item: inner, min: IntValue(0), max: IntValue(maxValidationSteps)}
	run := &validationRun{vm: vm}
	if err := run.value(list, ListValue(input), "", 0); err != nil || len(run.issues) != maxValidationIssues {
		t.Fatalf("expected issue cap: %d, %v", len(run.issues), err)
	}
	for i := range input {
		input[i] = BoolValue(true)
	}
	run = &validationRun{vm: vm}
	if err := run.value(list, ListValue(input), "", 0); err == nil || !strings.Contains(err.Error(), "work limit") {
		t.Fatalf("expected validation budget error, got %v", err)
	}
}
