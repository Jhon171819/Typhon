package typhon

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

const (
	maxValidationIssues = 64
	maxSchemaFields     = 256
	maxValidationSteps  = 100_000
)

type validationSchema struct {
	kind       string
	min, max   Value
	item       *validationSchema
	fields     []schemaField
	names      map[string]bool
	allowExtra bool
	options    map[string]bool
}

type schemaField struct {
	name     string
	required bool
	schema   *validationSchema
}

func validationTypes() map[string]Type {
	schema := Type{Kind: TypeNative, Name: "typhon.validation.Schema"}
	fields := dictType(StrType(), schema)
	length := []Type{IntType(), IntType()}
	intBound := unionType(IntType(), NoneType())
	numberBound := unionType(IntType(), FloatType(), NoneType())
	exports := map[string]Type{"Schema": schema}
	exports["text"] = nativeFuncType("validation.text", length, schema, 2)
	exports["integer"] = nativeFuncType("validation.integer", []Type{intBound, intBound}, schema, 2)
	exports["number"] = nativeFuncType("validation.number", []Type{numberBound, numberBound}, schema, 2)
	exports["boolean"] = nativeFuncType("validation.boolean", nil, schema, 0)
	exports["nullable"] = nativeFuncType("validation.nullable", []Type{schema}, schema, 0)
	exports["array"] = nativeFuncType("validation.array", []Type{schema, IntType(), IntType()}, schema, 2)
	exports["enumeration"] = nativeFuncType("validation.enumeration", []Type{listType(StrType())}, schema, 0)
	exports["object"] = nativeFuncType("validation.object", []Type{fields, fields, BoolType()}, schema, 2)
	exports["check"] = nativeFuncType("validation.check", []Type{schema, AnyType()}, listType(dictType(StrType(), StrType())), 0)
	return exports
}

func validationExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	for _, kind := range []string{"text", "integer", "number", "boolean", "nullable", "array", "enumeration", "object"} {
		exports[kind] = driverFunc(types[kind], func(vm *VM, args []Value) (Value, error) {
			schema, err := buildValidationSchema(vm, kind, args)
			if err != nil {
				return NoneValue(), err
			}
			value := nativeObject(vm, types["Schema"], nil)
			value.NativeObject.data = schema
			return value, nil
		})
	}
	exports["check"] = driverFunc(types["check"], func(vm *VM, args []Value) (Value, error) {
		schema, err := validationSchemaFrom(vm, args[0])
		if err != nil {
			return NoneValue(), err
		}
		check := validationRun{vm: vm}
		if err := check.value(schema, args[1], "", 0); err != nil {
			return NoneValue(), err
		}
		return ListValue(check.issues), nil
	})
	return exports
}

func validationSchemaFrom(vm *VM, value Value) (*validationSchema, error) {
	if value.NativeObject != nil {
		if err := vm.checkNativeOwner(value.NativeObject); err != nil {
			return nil, err
		}
		if schema, ok := value.NativeObject.data.(*validationSchema); ok {
			return schema, nil
		}
	}
	return nil, errors.New("expected a validation schema")
}

func buildValidationSchema(vm *VM, kind string, args []Value) (*validationSchema, error) {
	schema := &validationSchema{kind: kind, min: NoneValue(), max: NoneValue()}
	switch kind {
	case "nullable", "array":
		item, err := validationSchemaFrom(vm, args[0])
		if err != nil {
			return nil, err
		}
		schema.item = item
		if kind == "nullable" {
			return schema, nil
		}
		args = args[1:]
		fallthrough
	case "text":
		schema.min, schema.max = IntValue(0), IntValue(maxJSONBytes)
		if len(args) > 0 {
			schema.min = args[0]
		}
		if len(args) > 1 {
			schema.max = args[1]
		}
		if schema.min.Int < 0 || schema.max.Int < schema.min.Int {
			return nil, errors.New("length bounds must be non-negative and ordered")
		}
	case "integer", "number":
		if len(args) > 0 {
			schema.min = args[0]
		}
		if len(args) > 1 {
			schema.max = args[1]
		}
		if schema.min.Kind != ValueNone && schema.max.Kind != ValueNone {
			order, err := compareValues(schema.min, schema.max)
			if err != nil || order > 0 {
				return nil, errors.New("numeric bounds must be ordered")
			}
		}
	case "enumeration":
		values := *args[0].List
		if len(values) == 0 || len(values) > maxSchemaFields {
			return nil, errors.New("enumeration requires 1 to 256 strings")
		}
		schema.options = make(map[string]bool, len(values))
		for _, value := range values {
			if !utf8.ValidString(value.Str) || schema.options[value.Str] {
				return nil, errors.New("enumeration values must be unique UTF-8 strings")
			}
			schema.options[value.Str] = true
		}
	case "object":
		schema.names = map[string]bool{}
		schema.allowExtra = len(args) == 3 && args[2].Bool
		for i, fields := range args[:min(2, len(args))] {
			for _, entry := range fields.Dict.Entries {
				name := entry.Key.Str
				if len(schema.fields) >= maxSchemaFields || len(name) > 128 || !utf8.ValidString(name) || schema.names[name] {
					return nil, errors.New("schema fields must have unique UTF-8 names up to 128 bytes, at most 256 fields")
				}
				field, err := validationSchemaFrom(vm, entry.Value)
				if err != nil {
					return nil, err
				}
				schema.names[name] = true
				schema.fields = append(schema.fields, schemaField{name: name, required: i == 0, schema: field})
			}
		}
	}
	return schema, nil
}

type validationRun struct {
	vm     *VM
	steps  int
	issues []Value
}

func (v *validationRun) step() error {
	if err := v.vm.executionError(); err != nil {
		return err
	}
	v.steps++
	if v.steps > maxValidationSteps {
		return &Exception{kind: validationKind, message: "schema validation work limit exceeded"}
	}
	return v.vm.chargeWork()
}

func (v *validationRun) issue(path, rule, code, message string) {
	if len(v.issues) >= maxValidationIssues {
		return
	}
	if path == "" {
		path = "$"
	}
	item := DictValue()
	for _, field := range []struct{ key, value string }{
		{"path", path}, {"rule", rule}, {"code", code}, {"message", message},
	} {
		_ = item.Dict.put(StringValue(field.key), StringValue(field.value))
	}
	v.issues = append(v.issues, item)
}

func validationFieldPath(path, field string) string {
	simple := field != ""
	for i, ch := range field {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_' || i > 0 && ch >= '0' && ch <= '9') {
			simple = false
			break
		}
	}
	if !simple {
		return path + "[" + strconv.QuoteToGraphic(field) + "]"
	}
	if path == "" {
		return field
	}
	return path + "." + field
}

func (v *validationRun) value(s *validationSchema, input Value, path string, depth int) error {
	if len(v.issues) >= maxValidationIssues {
		return nil
	}
	if err := v.step(); err != nil {
		return err
	}
	if depth > maxJSONDepth || len(path) > 512 {
		return &Exception{kind: validationKind, message: "schema validation depth or path limit exceeded"}
	}
	switch s.kind {
	case "nullable":
		if input.Kind == ValueNone {
			return nil
		}
		return v.value(s.item, input, path, depth+1)
	case "boolean":
		if input.Kind != ValueBool {
			v.issue(path, "type", "invalid_type", "expected bool")
		}
	case "integer", "number":
		if input.Kind != ValueInt && (s.kind == "integer" || input.Kind != ValueFloat || math.IsInf(input.Float, 0) || math.IsNaN(input.Float)) {
			v.issue(path, "type", "invalid_type", "expected "+s.kind)
			return nil
		}
		if s.min.Kind != ValueNone {
			if order, _ := compareValues(input, s.min); order < 0 {
				v.issue(path, "minimum", "below_minimum", "number is below the minimum")
			}
		}
		if s.max.Kind != ValueNone {
			if order, _ := compareValues(input, s.max); order > 0 {
				v.issue(path, "maximum", "above_maximum", "number exceeds the maximum")
			}
		}
	case "text", "enumeration":
		if input.Kind != ValueString || !utf8.ValidString(input.Str) {
			v.issue(path, "type", "invalid_type", "expected UTF-8 text")
		} else if s.kind == "enumeration" {
			if !s.options[input.Str] {
				v.issue(path, "enum", "not_in_enum", "text is outside the allowed values")
			}
		} else {
			v.length(s, utf8.RuneCountInString(input.Str), path)
		}
	case "array":
		if input.Kind != ValueList || input.List == nil {
			v.issue(path, "type", "invalid_type", "expected an array")
			return nil
		}
		v.length(s, len(*input.List), path)
		for i, item := range *input.List {
			if len(v.issues) >= maxValidationIssues {
				break
			}
			if err := v.value(s.item, item, path+"["+strconv.Itoa(i)+"]", depth+1); err != nil {
				return err
			}
		}
	case "object":
		if input.Kind != ValueDict || input.Dict == nil {
			v.issue(path, "type", "invalid_type", "expected an object")
			return nil
		}
		if !s.allowExtra {
			for _, entry := range input.Dict.Entries {
				if err := v.step(); err != nil {
					return err
				}
				if entry.Key.Kind != ValueString || !s.names[entry.Key.Str] {
					v.issue(path, "additional_properties", "unexpected_field", "object contains an unexpected field")
					break
				}
			}
		}
		for _, field := range s.fields {
			if len(v.issues) >= maxValidationIssues {
				break
			}
			if err := v.step(); err != nil {
				return err
			}
			fieldPath := validationFieldPath(path, field.name)
			if len(fieldPath) > 512 {
				return &Exception{kind: validationKind, message: "schema validation path limit exceeded"}
			}
			index, exists, err := input.Dict.lookup(StringValue(field.name))
			if err != nil {
				return fmt.Errorf("schema field lookup: %w", err)
			}
			if !exists {
				if field.required {
					v.issue(fieldPath, "required", "missing_field", "required field is missing")
				}
				continue
			}
			if err := v.value(field.schema, input.Dict.Entries[index].Value, fieldPath, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validationRun) length(s *validationSchema, size int, path string) {
	if int64(size) < s.min.Int {
		v.issue(path, "min_length", "too_short", "value is shorter than the minimum")
	}
	if int64(size) > s.max.Int {
		v.issue(path, "max_length", "too_long", "value is longer than the maximum")
	}
}
