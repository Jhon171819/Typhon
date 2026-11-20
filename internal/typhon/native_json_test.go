package typhon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeJSONValues(t *testing.T) {
	for _, tc := range []struct {
		input  string
		kind   ValueKind
		output string
	}{
		{"null", ValueNone, "null"},
		{"true", ValueBool, "true"},
		{"9223372036854775807", ValueInt, "9223372036854775807"},
		{"-9223372036854775808", ValueInt, "-9223372036854775808"},
		{"9007199254740993", ValueInt, "9007199254740993"},
		{"1.25e1", ValueFloat, "12.5"},
		{` "\ud83d\ude80\n\u00e9" `, ValueString, `"🚀\né"`},
		{`{"z": [null, false, {"a":[]}], "a": 1}`, ValueDict, `{"z": [null, false, {"a": []}], "a": 1}`},
	} {
		value, err := loadJSON(nil, tc.input)
		if err != nil || value.Kind != tc.kind {
			t.Fatalf("load %q: %v, %v", tc.input, value.Kind, err)
		}
		result, err := dumpJSON(nil, value)
		if err != nil || result != tc.output {
			t.Errorf("dump %q: got %q (%v), expected %q", tc.input, result, err, tc.output)
		}
	}
}

func TestNativeJSONValidationAndLimits(t *testing.T) {
	for _, tc := range []struct{ text, reason string }{
		{"", "syntax"},
		{"[1,]", "syntax"},
		{"true null", "end of input"},
		{"NaN", "syntax"},
		{"Infinity", "syntax"},
		{"1e999", "finite"},
		{"9223372036854775808", "int64"},
		{`{"private-key":1,"private-key":2}`, "duplicate"},
		{`{"a":1,"\u0061":2}`, "duplicate"},
		{`"\uD800"`, "surrogate"},
		{`"\uDC00"`, "surrogate"},
		{`"\uD800\u0041"`, "surrogate"},
		{"\"" + "\xff" + "\"", "UTF-8"},
		{strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1), "depth limit"},
		{"[" + strings.Repeat("0,", maxJSONNodes) + "0]", "node limit"},
		{strings.Repeat(" ", maxJSONBytes+1), "byte limit"},
	} {
		_, err := loadJSON(nil, tc.text)
		var detail *JSONError
		var ex *Exception
		if !errors.As(err, &ex) || ex.Kind() != validationKind || !errors.As(err, &detail) ||
			detail.Offset < 1 || detail.Offset > int64(len(tc.text)+1) || !strings.Contains(detail.Reason, tc.reason) ||
			strings.Contains(err.Error(), "private-key") {
			t.Errorf("input length %d, expected %q: %v", len(tc.text), tc.reason, err)
		}
	}
	for _, value := range []Value{FloatValue(math.Inf(1)), FloatValue(math.NaN()), StringValue("\xff"), StringValue(strings.Repeat("x", maxJSONBytes))} {
		var detail *JSONError
		if _, err := dumpJSON(nil, value); !errors.As(err, &detail) || detail.Offset != 0 {
			t.Errorf("expected serialization error for %s, got %v", value.Kind, err)
		}
	}
	nodes := make([]Value, maxJSONNodes)
	for i := range nodes {
		nodes[i] = NoneValue()
	}
	if _, err := dumpJSON(nil, ListValue(nodes)); err == nil || !strings.Contains(err.Error(), "node limit") {
		t.Errorf("expected serialization node limit, got %v", err)
	}
	collision := DictValue()
	_ = collision.Dict.put(IntValue(1), NoneValue())
	_ = collision.Dict.put(StringValue("1"), NoneValue())
	if _, err := dumpJSON(nil, collision); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected collision error, got %v", err)
	}
}

func TestNativeJSONModuleAndBudget(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import json
from py.json import loads
result: int = loads('9007199254740993')
offset: int | None = None
try:
    json.loads('[')
except ValidationError as err:
    offset = err.offset
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, true); err != nil {
		t.Fatal(err)
	}
	if engine.bridge != nil || module.Globals["result"].Int != 9007199254740993 || module.Globals["offset"].Int != 2 {
		t.Fatal("JSON used Python, lost precision, or omitted the input position")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := loadJSON(&VM{ctx: cancelled}, "null"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decode: %v", err)
	}
	vm := &VM{engine: engine, instructions: 10_000_000}
	fn := jsonDriverExports(jsonDriverTypes())["loads"].Native
	_, err = callNativeDriver(vm, fn, []Value{StringValue("null")})
	var stopped *executionStop
	if !errors.As(err, &stopped) {
		t.Fatalf("VM budget was converted into a catchable error: %v", err)
	}
}

func TestNativeHTTPJSONCodec(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(sourceFile(t, `import json
import typhon.http as http
def handle(req: http.Request) -> http.Response:
    try:
        data: Any = req.json()
        if req.path == '/size':
            return http.json(200, len(data))
        response: http.Response = http.json(200, data)
        if response.body == json.dumps(data):
            return response
        return http.response(500, 'codec mismatch')
    except ValidationError:
        return http.response(400, 'invalid JSON')
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	handler := httpVMHandler(engine, module.Globals["handle"])
	for _, tc := range []struct {
		path, input, body string
		status            int
	}{
		{"/", `{"n":9007199254740993}`, `{"n": 9007199254740993}`, 200},
		{"/", "[" + strings.Repeat("0,", maxJSONNodes) + "0]", "invalid JSON", 400},
		{"/size", "\"" + strings.Repeat("x", maxHTTPBody-2) + "\"", fmt.Sprint(maxHTTPBody - 2), 200},
		{"/", strings.Repeat(" ", maxHTTPBody+1), "request body is too large or unreadable\n", 413},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.input)))
		if response.Code != tc.status || response.Body.String() != tc.body {
			t.Errorf("%s length %d: %d %q", tc.path, len(tc.input), response.Code, response.Body)
		}
	}
}

func FuzzNativeJSON(f *testing.F) {
	for _, seed := range []string{`{"a":[null,1,true]}`, `{"a":1,"a":2}`, `"\ud800"`, "\xff", strings.Repeat("[", 2000) + "null" + strings.Repeat("]", 2000)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		value, err := loadJSON(nil, text)
		if err != nil {
			return
		}
		if !json.Valid([]byte(text)) {
			t.Fatal("accepted invalid JSON")
		}
		if encoded, err := dumpJSON(nil, value); err == nil && !json.Valid([]byte(encoded)) {
			t.Fatal("serialized invalid JSON")
		}
	})
}
