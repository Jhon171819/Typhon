package typhon

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// validateValue enforces contracts at dynamic boundaries and preserves the
// element contract on mutable list aliases.
func validateValue(value Value, typ Type) (Value, error) {
	plan := &validationPlan{lists: map[*[]Value]*listContractUpdate{}}
	result, err := plan.validate(value, typ, 0)
	if err != nil {
		return NoneValue(), err
	}
	for _, update := range plan.lists {
		*update.value.List = update.items
		if update.value.ListGuard != nil {
			update.value.ListGuard.elem = update.elem
		}
	}
	return result, nil
}

type listContractUpdate struct {
	value Value
	elem  *Type
	items []Value
}
type validationPlan struct {
	lists map[*[]Value]*listContractUpdate
}

func (plan *validationPlan) validate(value Value, typ Type, depth int) (Value, error) {
	if depth > 64 {
		return NoneValue(), fmt.Errorf("type validation depth limit exceeded")
	}
	if typ.Kind == "" || typ.Kind == TypeAny || typ.Kind == TypePy {
		return value, nil
	}
	if typ.Kind == TypeUnion {
		for _, option := range typ.Options {
			trial := &validationPlan{lists: map[*[]Value]*listContractUpdate{}}
			for key, update := range plan.lists {
				trial.lists[key] = update
			}
			if result, err := trial.validate(value, option, depth+1); err == nil {
				plan.lists = trial.lists
				return result, nil
			}
		}
		return NoneValue(), fmt.Errorf("expected %s, got %s", typ, value.Kind)
	}
	ok := false
	switch typ.Kind {
	case TypeNone:
		ok = value.Kind == ValueNone
	case TypeInt:
		ok = value.Kind == ValueInt
	case TypeFloat:
		if value.Kind == ValueInt {
			return FloatValue(float64(value.Int)), nil
		}
		ok = value.Kind == ValueFloat && !math.IsInf(value.Float, 0) && !math.IsNaN(value.Float)
	case TypeBool:
		ok = value.Kind == ValueBool
	case TypeStr:
		ok = value.Kind == ValueString
	case TypeLiteral:
		ok = value.Kind == ValueString && value.Str == typ.Name
	case TypeTask:
		ok = value.Kind == ValueTask
	case TypeClass:
		ok = value.Kind == ValueObject && value.Object != nil && value.Object.Class.Spec.Info == typ.Class
	case TypeFunc:
		ok = value.Kind == ValueFunction && value.Function.Signature.Equal(typ)
	case TypeList:
		if value.Kind == ValueList && value.List != nil && typ.Elem != nil {
			if update, ok := plan.lists[value.List]; ok {
				if !update.elem.Equal(*typ.Elem) {
					return NoneValue(), fmt.Errorf("incompatible contracts for aliased list")
				}
				return value, nil
			}
			if value.ListGuard != nil && value.ListGuard.elem != nil {
				if !value.ListGuard.elem.Equal(*typ.Elem) {
					return NoneValue(), fmt.Errorf("incompatible mutable list contract: %s and %s", value.ListGuard.elem, typ.Elem)
				}
				return value, nil
			}
			items := make([]Value, len(*value.List))
			plan.lists[value.List] = &listContractUpdate{value: value, elem: typ.Elem, items: items}
			for i, item := range *value.List {
				converted, err := plan.validate(item, *typ.Elem, depth+1)
				if err != nil {
					return NoneValue(), fmt.Errorf("list element %d: %w", i, err)
				}
				items[i] = converted
			}
			return value, nil
		}
	}
	if !ok {
		return NoneValue(), fmt.Errorf("expected %s, got %s", typ, value.Kind)
	}
	return value, nil
}

const maxCollectionItems = 1_000_000
const maxStringBytes = 16 * 1024 * 1024

func arithmetic(op Op, left, right Value) (Value, error) {
	if op == OpAdd && left.Kind == ValueString && right.Kind == ValueString {
		if len(left.Str) > maxStringBytes-len(right.Str) {
			return NoneValue(), fmt.Errorf("string size limit exceeded")
		}
		return StringValue(left.Str + right.Str), nil
	}
	if op == OpMul && (left.Kind == ValueString && right.Kind == ValueInt || right.Kind == ValueString && left.Kind == ValueInt) {
		if right.Kind == ValueString {
			left, right = right, left
		}
		if right.Int < 0 {
			return NoneValue(), fmt.Errorf("string repetition count must not be negative")
		}
		if left.Str == "" {
			return StringValue(""), nil
		}
		if right.Int > int64(maxStringBytes/len(left.Str)) {
			return NoneValue(), fmt.Errorf("string size limit exceeded")
		}
		return StringValue(strings.Repeat(left.Str, int(right.Int))), nil
	}
	if (left.Kind != ValueInt && left.Kind != ValueFloat) || (right.Kind != ValueInt && right.Kind != ValueFloat) {
		return NoneValue(), fmt.Errorf("operator %s does not support %s and %s", op, left.Kind, right.Kind)
	}
	if left.Kind == ValueFloat || right.Kind == ValueFloat || op == OpDiv {
		a, b := numberValue(left), numberValue(right)
		if (op == OpDiv || op == OpFloorDiv || op == OpMod) && b == 0 {
			return NoneValue(), fmt.Errorf("division by zero")
		}
		var result float64
		switch op {
		case OpDiv:
			result = a / b
		case OpFloorDiv:
			result = math.Floor(a / b)
		case OpMod:
			result = math.Mod(a, b)
			if result != 0 && math.Signbit(result) != math.Signbit(b) {
				result += b
			}
		case OpAdd:
			result = a + b
		case OpSub:
			result = a - b
		case OpMul:
			result = a * b
		}
		if math.IsInf(result, 0) || math.IsNaN(result) {
			return NoneValue(), fmt.Errorf("non-finite arithmetic result")
		}
		return FloatValue(result), nil
	}
	a, b := left.Int, right.Int
	if op == OpFloorDiv || op == OpMod {
		if b == 0 {
			return NoneValue(), fmt.Errorf("division by zero")
		}
		if a == math.MinInt64 && b == -1 {
			if op == OpMod {
				return IntValue(0), nil
			}
			return NoneValue(), fmt.Errorf("int64 overflow in floor division")
		}
		q, r := a/b, a%b
		if r != 0 && (r < 0) != (b < 0) {
			q--
			r += b
		}
		if op == OpMod {
			return IntValue(r), nil
		}
		return IntValue(q), nil
	}
	var result int64
	overflow := false
	switch op {
	case OpAdd:
		result = a + b
		overflow = b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b
	case OpSub:
		result = a - b
		overflow = b < 0 && a > math.MaxInt64+b || b > 0 && a < math.MinInt64+b
	case OpMul:
		result = a * b
		overflow = a != 0 && (a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 || result/a != b)
	}
	if overflow {
		return NoneValue(), fmt.Errorf("int64 overflow in %s", op)
	}
	return IntValue(result), nil
}

func compareValues(left, right Value) (int, error) {
	if left.Kind == ValueString && right.Kind == ValueString {
		return strings.Compare(left.Str, right.Str), nil
	}
	if left.Kind == ValueInt && right.Kind == ValueInt {
		if left.Int < right.Int {
			return -1, nil
		}
		if left.Int > right.Int {
			return 1, nil
		}
		return 0, nil
	}
	if (left.Kind == ValueInt || left.Kind == ValueFloat) && (right.Kind == ValueInt || right.Kind == ValueFloat) {
		convert := func(v Value) *big.Rat {
			if v.Kind == ValueInt {
				return new(big.Rat).SetInt64(v.Int)
			}
			return new(big.Rat).SetFloat64(v.Float)
		}
		a, b := convert(left), convert(right)
		if a == nil || b == nil {
			return 0, fmt.Errorf("cannot compare non-finite numbers")
		}
		return a.Cmp(b), nil
	}
	return 0, fmt.Errorf("cannot compare %s and %s", left.Kind, right.Kind)
}
