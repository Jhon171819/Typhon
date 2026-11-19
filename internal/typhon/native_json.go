package typhon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxJSONBytes = 8 << 20
	maxJSONDepth = 64
	maxJSONNodes = 100_000
)

// JSONError carries a one-based input byte position. Zero means the error
// occurred while serializing a value.
type JSONError struct {
	Offset int64
	Reason string
}

func (e *JSONError) Error() string {
	if e.Offset > 0 {
		return fmt.Sprintf("JSON byte %d: %s", e.Offset, e.Reason)
	}
	return "JSON: " + e.Reason
}

func invalidJSON(offset int64, reason string) error {
	cause := &JSONError{Offset: offset, Reason: reason}
	return &Exception{kind: validationKind, message: cause.Error(), cause: cause}
}

func jsonDriverTypes() map[string]Type {
	return map[string]Type{
		"loads": nativeFuncType("json.loads", []Type{StrType()}, AnyType(), 0),
		"dumps": nativeFuncType("json.dumps", []Type{AnyType()}, StrType(), 0),
	}
}

func jsonDriverExports(types map[string]Type) map[string]Value {
	return map[string]Value{
		"loads": driverFunc(types["loads"], func(vm *VM, args []Value) (Value, error) {
			return loadJSON(vm, args[0].Str)
		}),
		"dumps": driverFunc(types["dumps"], func(vm *VM, args []Value) (Value, error) {
			text, err := dumpJSON(vm, args[0])
			return StringValue(text), err
		}),
	}
}

type jsonBudget struct {
	vm    *VM
	nodes int
}

func (b *jsonBudget) visit(offset int64, depth int) error {
	if depth > maxJSONDepth {
		return invalidJSON(offset, "depth limit exceeded")
	}
	b.nodes++
	if b.nodes > maxJSONNodes {
		return invalidJSON(offset, "node limit exceeded")
	}
	if b.vm != nil {
		if err := b.vm.executionError(); err != nil {
			return err
		}
		return b.vm.chargeWork()
	}
	return nil
}

func loadJSON(vm *VM, text string) (Value, error) {
	if len(text) > maxJSONBytes {
		return NoneValue(), invalidJSON(maxJSONBytes+1, "byte limit exceeded")
	}
	if vm != nil {
		if err := vm.chargeBytes(int64(len(text))); err != nil {
			return NoneValue(), err
		}
	}
	if err := checkJSONUnicode(vm, text); err != nil {
		return NoneValue(), err
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	budget := jsonBudget{vm: vm}
	result, err := readJSON(decoder, &budget, 0)
	if err != nil {
		return NoneValue(), err
	}
	offset := decoder.InputOffset() + 1
	if _, err := decoder.Token(); err != io.EOF {
		return NoneValue(), invalidJSON(offset, "expected end of input")
	}
	return result, nil
}

// encoding/json replaces invalid Unicode. Typhon rejects it before decoding,
// including unpaired surrogate escapes, so keys never change silently.
func checkJSONUnicode(vm *VM, text string) error {
	inString := false
	for i := 0; i < len(text); {
		if vm != nil && i%4096 < 12 {
			if err := vm.executionError(); err != nil {
				return err
			}
		}
		ch := text[i]
		switch {
		case ch >= utf8.RuneSelf:
			_, size := utf8.DecodeRuneInString(text[i:])
			if size == 1 {
				return invalidJSON(int64(i+1), "invalid UTF-8")
			}
			i += size
		case ch == '"':
			inString = !inString
			i++
		case ch == '\\' && inString && i+1 < len(text):
			if text[i+1] != 'u' || i+6 > len(text) {
				i += 2
				continue
			}
			code, err := strconv.ParseUint(text[i+2:i+6], 16, 16)
			if err != nil {
				return invalidJSON(int64(i+1), "invalid Unicode escape")
			}
			if code < 0xD800 || code > 0xDFFF {
				i += 6
				continue
			}
			if code >= 0xDC00 || i+12 > len(text) || text[i+6:i+8] != "\\u" {
				return invalidJSON(int64(i+1), "unpaired Unicode surrogate")
			}
			low, err := strconv.ParseUint(text[i+8:i+12], 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return invalidJSON(int64(i+1), "unpaired Unicode surrogate")
			}
			i += 12
		default:
			i++
		}
	}
	return nil
}

func jsonToken(d *json.Decoder) (json.Token, error) {
	token, err := d.Token()
	if err == nil {
		return token, nil
	}
	offset := d.InputOffset() + 1
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) && syntax.Offset > offset {
		offset = syntax.Offset
	}
	return nil, invalidJSON(offset, "invalid JSON syntax")
}

func readJSON(d *json.Decoder, budget *jsonBudget, depth int) (Value, error) {
	offset := d.InputOffset() + 1
	if err := budget.visit(offset, depth); err != nil {
		return NoneValue(), err
	}
	if budget.vm != nil {
		if err := budget.vm.chargeBytes(valueSlotBytes); err != nil {
			return NoneValue(), err
		}
	}
	token, err := jsonToken(d)
	if err != nil {
		return NoneValue(), err
	}
	switch token := token.(type) {
	case nil:
		return NoneValue(), nil
	case bool:
		return BoolValue(token), nil
	case string:
		return StringValue(token), nil
	case json.Number:
		if !strings.ContainsAny(string(token), ".eE") {
			n, err := token.Int64()
			if err != nil {
				return NoneValue(), invalidJSON(offset, "integer outside int64")
			}
			return IntValue(n), nil
		}
		n, err := token.Float64()
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return NoneValue(), invalidJSON(offset, "number outside finite float64")
		}
		return FloatValue(n), nil
	case json.Delim:
		switch token {
		case '[':
			var items []Value
			for d.More() {
				item, err := readJSON(d, budget, depth+1)
				if err != nil {
					return NoneValue(), err
				}
				items = append(items, item)
			}
			_, err := jsonToken(d)
			return ListValue(items), err
		case '{':
			object := DictValue()
			for d.More() {
				keyOffset := d.InputOffset() + 1
				if err := budget.visit(keyOffset, depth+1); err != nil {
					return NoneValue(), err
				}
				if budget.vm != nil {
					if err := budget.vm.chargeBytes(2 * valueSlotBytes); err != nil {
						return NoneValue(), err
					}
				}
				keyToken, err := jsonToken(d)
				if err != nil {
					return NoneValue(), err
				}
				key, ok := keyToken.(string)
				if !ok {
					return NoneValue(), invalidJSON(keyOffset, "expected object key")
				}
				k := StringValue(key)
				if _, found, _ := object.Dict.lookup(k); found {
					return NoneValue(), invalidJSON(keyOffset, "duplicate object key")
				}
				item, err := readJSON(d, budget, depth+1)
				if err != nil {
					return NoneValue(), err
				}
				if err := object.Dict.put(k, item); err != nil {
					return NoneValue(), invalidJSON(keyOffset, "object size limit exceeded")
				}
			}
			_, err := jsonToken(d)
			return object, err
		}
	}
	return NoneValue(), invalidJSON(offset, "expected JSON value")
}

type jsonWriter struct {
	jsonBudget
	text strings.Builder
}

func dumpJSON(vm *VM, value Value) (string, error) {
	w := jsonWriter{jsonBudget: jsonBudget{vm: vm}}
	if err := w.value(value, 0); err != nil {
		return "", err
	}
	return w.text.String(), nil
}

func (w *jsonWriter) write(s string) error {
	if len(s) > maxJSONBytes-w.text.Len() {
		return invalidJSON(0, "byte limit exceeded")
	}
	if w.vm != nil {
		if err := w.vm.chargeBytes(int64(len(s))); err != nil {
			return err
		}
	}
	w.text.WriteString(s)
	return nil
}

func (w *jsonWriter) string(s string) error {
	if len(s) > maxJSONBytes-w.text.Len() {
		return invalidJSON(0, "byte limit exceeded")
	}
	if !utf8.ValidString(s) {
		return invalidJSON(0, "invalid UTF-8")
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return invalidJSON(0, "invalid string")
	}
	return w.write(string(encoded))
}

func jsonScalar(value Value) (string, error) {
	switch value.Kind {
	case ValueNone:
		return "null", nil
	case ValueBool:
		return strconv.FormatBool(value.Bool), nil
	case ValueInt:
		return strconv.FormatInt(value.Int, 10), nil
	case ValueFloat:
		if math.IsInf(value.Float, 0) || math.IsNaN(value.Float) {
			return "", invalidJSON(0, "non-finite number")
		}
		return strconv.FormatFloat(value.Float, 'f', -1, 64), nil
	case ValueString:
		return value.Str, nil
	default:
		return "", invalidJSON(0, "unsupported value type "+string(value.Kind))
	}
}

func (w *jsonWriter) value(value Value, depth int) error {
	if err := w.visit(0, depth); err != nil {
		return err
	}
	switch value.Kind {
	case ValueString:
		return w.string(value.Str)
	case ValueList:
		if value.List == nil || len(*value.List) == 0 {
			return w.write("[]")
		}
		if err := w.write("["); err != nil {
			return err
		}
		for i, item := range *value.List {
			if err := w.separator(i); err != nil {
				return err
			}
			if err := w.value(item, depth+1); err != nil {
				return err
			}
		}
		return w.write("]")
	case ValueDict:
		if value.Dict == nil || len(value.Dict.Entries) == 0 {
			return w.write("{}")
		}
		if err := w.write("{"); err != nil {
			return err
		}
		seen := map[string]bool{}
		for i, entry := range value.Dict.Entries {
			if err := w.visit(0, depth+1); err != nil {
				return err
			}
			key, err := jsonScalar(entry.Key)
			if err != nil {
				return err
			}
			if seen[key] {
				return invalidJSON(0, "duplicate object key after conversion")
			}
			seen[key] = true
			if err := w.separator(i); err != nil {
				return err
			}
			if err := w.string(key); err != nil {
				return err
			}
			if err := w.write(": "); err != nil {
				return err
			}
			if err := w.value(entry.Value, depth+1); err != nil {
				return err
			}
		}
		return w.write("}")
	default:
		text, err := jsonScalar(value)
		if err != nil {
			return err
		}
		return w.write(text)
	}
}

func (w *jsonWriter) separator(index int) error {
	if index == 0 {
		return nil
	}
	return w.write(", ")
}
