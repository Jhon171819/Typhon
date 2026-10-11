package typhon

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

func dictType(key, elem Type) Type {
	return Type{Kind: TypeDict, Name: "dict", Key: typePtr(key), Elem: typePtr(elem)}
}

func widenLiteral(t Type) Type {
	if t.Kind == TypeLiteral {
		return StrType()
	}
	return t
}

func hashableType(t Type) bool {
	switch t.Kind {
	case TypeInt, TypeFloat, TypeStr, TypeLiteral, TypeBool, TypeNone, TypeAny, TypePy, TypeNever:
		return true
	case TypeUnion:
		for _, option := range t.Options {
			if !hashableType(option) {
				return false
			}
		}
		return true
	}
	return false
}

func (c *checker) inferIndex(e *IndexExpr, scope *scope, class *ClassInfo) Type {
	obj := c.inferExpr(e.Object, scope, class)
	key := c.inferExpr(e.Index, scope, class)
	switch obj.Kind {
	case TypeDict:
		if obj.Key != nil && obj.Key.Kind != TypeNever && !assignable(key, *obj.Key) {
			c.fail(e.Line, fmt.Sprintf("dictionary key expected %s, got %s", obj.Key, key))
		}
		if obj.Elem != nil {
			return *obj.Elem
		}
	case TypeList, TypeStr, TypeLiteral:
		if !assignable(key, IntType()) {
			c.fail(e.Line, "sequence index must be int")
		}
		if obj.Kind != TypeList {
			return StrType()
		}
		if obj.Elem != nil {
			return *obj.Elem
		}
	case TypeAny, TypePy:
		return AnyType()
	default:
		c.fail(e.Line, fmt.Sprintf("%s does not support indexing", obj))
	}
	return AnyType()
}

// Entries preserve insertion order; the index provides constant-time key lookup.
type Dictionary struct {
	Entries  []DictEntry
	Index    map[dictKey]int
	Contract *Type
}

type DictEntry struct{ Key, Value Value }
type dictKey struct {
	Kind  ValueKind
	Int   int64
	Float float64
	Str   string
}

func dictionaryKey(v Value) (dictKey, error) {
	switch v.Kind {
	case ValueNone:
		return dictKey{Kind: ValueNone}, nil
	case ValueString:
		return dictKey{Kind: ValueString, Str: v.Str}, nil
	case ValueBool:
		if v.Bool {
			return dictKey{Kind: ValueInt, Int: 1}, nil
		}
		return dictKey{Kind: ValueInt}, nil
	case ValueInt:
		return dictKey{Kind: ValueInt, Int: v.Int}, nil
	case ValueFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return dictKey{}, fmt.Errorf("dictionary key must be finite")
		}
		if v.Float >= -9223372036854775808 && v.Float < 9223372036854775808 && math.Trunc(v.Float) == v.Float {
			return dictKey{Kind: ValueInt, Int: int64(v.Float)}, nil
		}
		return dictKey{Kind: ValueFloat, Float: v.Float}, nil
	default:
		return dictKey{}, fmt.Errorf("unhashable dictionary key: %s", v.Kind)
	}
}

func DictValue() Value {
	return Value{Kind: ValueDict, Dict: &Dictionary{Index: map[dictKey]int{}}}
}

func (d *Dictionary) lookup(key Value) (int, bool, error) {
	k, err := dictionaryKey(key)
	if err != nil {
		return 0, false, err
	}
	i, ok := d.Index[k]
	return i, ok, nil
}

func (d *Dictionary) put(key, value Value) error {
	k, err := dictionaryKey(key)
	if err != nil {
		return err
	}
	if i, ok := d.Index[k]; ok {
		d.Entries[i].Value = value
		return nil
	}
	if len(d.Entries) >= maxCollectionItems {
		return fmt.Errorf("dictionary size limit exceeded")
	}
	d.Index[k] = len(d.Entries)
	d.Entries = append(d.Entries, DictEntry{Key: key, Value: value})
	return nil
}

func (d *Dictionary) remove(i int) Value {
	entry := d.Entries[i]
	k, _ := dictionaryKey(entry.Key)
	delete(d.Index, k)
	copy(d.Entries[i:], d.Entries[i+1:])
	d.Entries[len(d.Entries)-1] = DictEntry{}
	d.Entries = d.Entries[:len(d.Entries)-1]
	for j := i; j < len(d.Entries); j++ {
		k, _ := dictionaryKey(d.Entries[j].Key)
		d.Index[k] = j
	}
	return entry.Value
}

func (d *Dictionary) checkKey(key Value) (Value, error) {
	if _, err := dictionaryKey(key); err != nil {
		return NoneValue(), err
	}
	if d.Contract != nil {
		return validateValue(key, *d.Contract.Key)
	}
	return key, nil
}

func setIndex(obj, key, value Value) error {
	if obj.Kind == ValueDict && obj.Dict != nil {
		if obj.Dict.Contract != nil {
			plan := newValidationPlan()
			var err error
			key, err = plan.validate(key, *obj.Dict.Contract.Key, 0)
			if err != nil {
				return fmt.Errorf("dictionary key: %w", err)
			}
			value, err = plan.validate(value, *obj.Dict.Contract.Elem, 0)
			if err != nil {
				return fmt.Errorf("dictionary value: %w", err)
			}
			if _, exists, err := obj.Dict.lookup(key); err != nil {
				return err
			} else if !exists && len(obj.Dict.Entries) >= maxCollectionItems {
				return fmt.Errorf("dictionary size limit exceeded")
			}
			plan.commit()
		}
		return obj.Dict.put(key, value)
	}
	if obj.Kind == ValueList && obj.List != nil {
		i, err := sequenceIndex(key, len(*obj.List))
		if err != nil {
			return err
		}
		if obj.ListGuard != nil && obj.ListGuard.elem != nil {
			value, err = validateValue(value, *obj.ListGuard.elem)
			if err != nil {
				return err
			}
		}
		(*obj.List)[i] = value
		return nil
	}
	return fmt.Errorf("%s does not support index assignment", obj.Kind)
}

func sequenceIndex(key Value, size int) (int, error) {
	if key.Kind != ValueInt {
		return 0, fmt.Errorf("sequence index must be int")
	}
	i := key.Int
	if i < 0 {
		i += int64(size)
	}
	if i < 0 || i >= int64(size) {
		return 0, fmt.Errorf("index out of range")
	}
	return int(i), nil
}

func getIndex(obj, key Value) (Value, error) {
	switch obj.Kind {
	case ValueDict:
		if obj.Dict == nil {
			break
		}
		var err error
		key, err = obj.Dict.checkKey(key)
		if err != nil {
			return NoneValue(), err
		}
		i, ok, err := obj.Dict.lookup(key)
		if err != nil {
			return NoneValue(), err
		}
		if !ok {
			return NoneValue(), fmt.Errorf("KeyError: %s", key.String())
		}
		return obj.Dict.Entries[i].Value, nil
	case ValueList:
		if obj.List == nil {
			break
		}
		i, err := sequenceIndex(key, len(*obj.List))
		if err != nil {
			return NoneValue(), err
		}
		return (*obj.List)[i], nil
	case ValueString:
		chars := []rune(obj.Str)
		i, err := sequenceIndex(key, len(chars))
		if err != nil {
			return NoneValue(), err
		}
		return StringValue(string(chars[i])), nil
	}
	return NoneValue(), fmt.Errorf("%s does not support indexing", obj.Kind)
}

func collectionValues(obj Value) ([]Value, error) {
	if obj.Kind == ValueList && obj.List != nil {
		return *obj.List, nil
	}
	if obj.Kind == ValueDict && obj.Dict != nil {
		keys := make([]Value, len(obj.Dict.Entries))
		for i, entry := range obj.Dict.Entries {
			keys[i] = entry.Key
		}
		return keys, nil
	}
	return nil, fmt.Errorf("iteration expected list or dict, got %s", obj.Kind)
}

func containsValue(obj, key Value) (bool, error) {
	switch obj.Kind {
	case ValueDict:
		if obj.Dict == nil {
			break
		}
		var err error
		key, err = obj.Dict.checkKey(key)
		if err != nil {
			return false, err
		}
		_, ok, err := obj.Dict.lookup(key)
		return ok, err
	case ValueList:
		if obj.List != nil {
			for _, item := range *obj.List {
				if valuesEqual(item, key) {
					return true, nil
				}
			}
		}
		return false, nil
	case ValueString:
		if key.Kind != ValueString {
			return false, fmt.Errorf("string membership requires str")
		}
		return strings.Contains(obj.Str, key.Str), nil
	}
	return false, fmt.Errorf("%s does not support membership", obj.Kind)
}

func nativeLen(vm *VM, args []Value) (Value, error) {
	if len(args) != 1 {
		return NoneValue(), fmt.Errorf("len expected 1 argument")
	}
	v := args[0]
	switch v.Kind {
	case ValueDict:
		if v.Dict != nil {
			return IntValue(int64(len(v.Dict.Entries))), nil
		}
	case ValueList:
		if v.List != nil {
			return IntValue(int64(len(*v.List))), nil
		}
	case ValueString:
		return IntValue(int64(utf8.RuneCountInString(v.Str))), nil
	}
	return NoneValue(), fmt.Errorf("len does not support %s", v.Kind)
}

func nativeDict(vm *VM, args []Value) (Value, error) {
	if len(args) == 0 {
		return DictValue(), nil
	}
	if len(args) != 1 || args[0].Kind != ValueDict || args[0].Dict == nil {
		return NoneValue(), fmt.Errorf("dict expected 0 arguments or a dictionary")
	}
	copy := DictValue()
	for _, entry := range args[0].Dict.Entries {
		if err := copy.Dict.put(entry.Key, entry.Value); err != nil {
			return NoneValue(), err
		}
	}
	copy.Dict.Contract = args[0].Dict.Contract
	return copy, nil
}

func dictMethod(receiver Value, name string) (Value, bool) {
	switch name {
	case "get", "keys", "values", "items", "pop", "clear", "copy", "update", "setdefault":
		return boundNative(receiver, "dict."+name, func(vm *VM, args []Value) (Value, error) {
			d := args[0].Dict
			if d == nil {
				return NoneValue(), fmt.Errorf("invalid dictionary")
			}
			args = args[1:]
			minArgs, maxArgs := 0, 0
			switch name {
			case "get", "pop", "setdefault":
				minArgs, maxArgs = 1, 2
			case "update":
				minArgs, maxArgs = 1, 1
			}
			if len(args) < minArgs || len(args) > maxArgs {
				return NoneValue(), fmt.Errorf("dict.%s expected %d to %d arguments", name, minArgs, maxArgs)
			}
			switch name {
			case "get", "pop", "setdefault":
				key, err := d.checkKey(args[0])
				if err != nil {
					return NoneValue(), err
				}
				i, ok, err := d.lookup(key)
				if err != nil {
					return NoneValue(), err
				}
				if ok {
					if name == "pop" {
						return d.remove(i), nil
					}
					return d.Entries[i].Value, nil
				}
				fallback := NoneValue()
				if len(args) == 2 {
					fallback = args[1]
				}
				if name == "pop" && len(args) == 1 {
					return NoneValue(), fmt.Errorf("KeyError: %s", args[0].String())
				}
				if name == "setdefault" {
					if err := setIndex(receiver, key, fallback); err != nil {
						return NoneValue(), err
					}
					return getIndex(receiver, key)
				}
				return fallback, nil
			case "copy":
				return nativeDict(vm, []Value{receiver})
			case "clear":
				d.Entries = nil
				d.Index = map[dictKey]int{}
			case "update":
				if args[0].Kind != ValueDict || args[0].Dict == nil {
					return NoneValue(), fmt.Errorf("dict.update requires a dictionary")
				}
				candidate, err := nativeDict(vm, []Value{receiver})
				if err != nil {
					return NoneValue(), err
				}
				candidate.Dict.Contract = nil
				for _, entry := range args[0].Dict.Entries {
					if err := candidate.Dict.put(entry.Key, entry.Value); err != nil {
						return NoneValue(), err
					}
				}
				if d.Contract != nil {
					if _, err := validateValue(candidate, *d.Contract); err != nil {
						return NoneValue(), err
					}
				}
				d.Entries, d.Index = candidate.Dict.Entries, candidate.Dict.Index
			default:
				items := make([]Value, len(d.Entries))
				for i, entry := range d.Entries {
					switch name {
					case "keys":
						items[i] = entry.Key
					case "values":
						items[i] = entry.Value
					case "items":
						items[i] = ListValue([]Value{entry.Key, entry.Value})
					}
				}
				return ListValue(items), nil
			}
			return NoneValue(), nil
		}), true
	}
	return NoneValue(), false
}
