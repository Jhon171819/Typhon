package typhon

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

const maxPostgresArrayItems = 4096

type postgresParameter struct{ data any }

func postgresParameterType() Type { return Type{Kind: TypeNative, Name: "typhon.postgres.Parameter"} }

func postgresParamTypes(types map[string]Type) {
	param := postgresParameterType()
	types["Parameter"] = param
	types["jsonb"] = nativeFuncType("postgres.jsonb", []Type{AnyType()}, param, 0)
	types["array"] = nativeFuncType("postgres.array", []Type{StrType(), AnyType()}, param, 0)
	types["null"] = nativeFuncType("postgres.null", []Type{StrType()}, param, 0)
}

func postgresParamExports(exports map[string]Value, types map[string]Type) {
	makeParam := func(vm *VM, data any) (Value, error) {
		value := nativeObject(vm, types["Parameter"], nil)
		value.NativeObject.data = &postgresParameter{data: data}
		return value, nil
	}
	exports["jsonb"] = driverFunc(types["jsonb"], func(vm *VM, args []Value) (Value, error) {
		data, err := postgresJSONParam(vm, args[0])
		if err != nil {
			return NoneValue(), err
		}
		return makeParam(vm, data)
	})
	exports["array"] = driverFunc(types["array"], func(vm *VM, args []Value) (Value, error) {
		kind := postgresScalarOID(args[0].Str)
		if kind == 0 || kind == pgtype.JSONBOID || args[1].Kind != ValueList {
			return NoneValue(), errors.New("PostgreSQL array requires a supported scalar element type and a list")
		}
		values := *args[1].List
		if len(values) > maxPostgresArrayItems {
			return NoneValue(), errors.New("PostgreSQL array exceeds 4096 elements")
		}
		if err := vm.chargeBytes(int64(len(values)) * valueSlotBytes); err != nil {
			return NoneValue(), err
		}
		items := make([]any, len(values))
		for i, value := range values {
			if err := vm.executionError(); err != nil {
				return NoneValue(), err
			}
			if value.Kind == ValueNone {
				continue
			}
			item, err := postgresArrayItem(vm, kind, value)
			if err != nil {
				return NoneValue(), err
			}
			items[i] = item
		}
		return makeParam(vm, pgtype.Array[any]{
			Elements: items, Dims: []pgtype.ArrayDimension{{Length: int32(len(items)), LowerBound: 1}}, Valid: true,
		})
	})
	exports["null"] = driverFunc(types["null"], func(vm *VM, args []Value) (Value, error) {
		name, array := strings.CutSuffix(args[0].Str, "[]")
		kind := postgresScalarOID(name)
		if kind == 0 || array && kind == pgtype.JSONBOID {
			return NoneValue(), errors.New("unsupported PostgreSQL null type")
		}
		if array {
			return makeParam(vm, pgtype.Array[any]{})
		}
		return makeParam(vm, postgresNullValue(kind))
	})
}

func postgresJSONParam(vm *VM, value Value) (json.RawMessage, error) {
	text, err := dumpJSON(vm, value)
	if err != nil {
		return nil, err
	}
	if err := vm.chargeBytes(int64(len(text))); err != nil {
		return nil, err
	}
	return json.RawMessage(text), nil
}

func postgresScalarOID(name string) uint32 {
	switch name {
	case "int4":
		return pgtype.Int4OID
	case "int8":
		return pgtype.Int8OID
	case "text":
		return pgtype.TextOID
	case "bool":
		return pgtype.BoolOID
	case "float8":
		return pgtype.Float8OID
	case "numeric":
		return pgtype.NumericOID
	case "uuid":
		return pgtype.UUIDOID
	case "date":
		return pgtype.DateOID
	case "timestamp":
		return pgtype.TimestampOID
	case "timestamptz":
		return pgtype.TimestamptzOID
	case "bytea":
		return pgtype.ByteaOID
	case "jsonb":
		return pgtype.JSONBOID
	default:
		return 0
	}
}

func postgresNullValue(oid uint32) any {
	switch oid {
	case pgtype.Int4OID:
		return pgtype.Int4{}
	case pgtype.Int8OID:
		return pgtype.Int8{}
	case pgtype.TextOID:
		return pgtype.Text{}
	case pgtype.BoolOID:
		return pgtype.Bool{}
	case pgtype.Float8OID:
		return pgtype.Float8{}
	case pgtype.NumericOID:
		return pgtype.Numeric{}
	case pgtype.UUIDOID:
		return pgtype.UUID{}
	case pgtype.DateOID:
		return pgtype.Date{}
	case pgtype.TimestampOID:
		return pgtype.Timestamp{}
	case pgtype.TimestamptzOID:
		return pgtype.Timestamptz{}
	case pgtype.ByteaOID:
		return []byte(nil)
	case pgtype.JSONBOID:
		return json.RawMessage(nil)
	default:
		return nil
	}
}

func postgresArrayItem(vm *VM, oid uint32, value Value) (any, error) {
	switch oid {
	case pgtype.Int4OID:
		if value.Kind == ValueInt && value.Int >= math.MinInt32 && value.Int <= math.MaxInt32 {
			return int32(value.Int), nil
		}
	case pgtype.Int8OID:
		if value.Kind == ValueInt {
			return value.Int, nil
		}
	case pgtype.TextOID:
		if value.Kind == ValueString {
			return value.Str, nil
		}
	case pgtype.BoolOID:
		if value.Kind == ValueBool {
			return value.Bool, nil
		}
	case pgtype.Float8OID:
		if value.Kind == ValueFloat && !math.IsNaN(value.Float) && !math.IsInf(value.Float, 0) {
			return value.Float, nil
		}
	default:
		if value.Kind != ValueNativeObject {
			break
		}
		scalar, err := postgresScalarParam(vm, value)
		if err != nil {
			return nil, err
		}
		var expected uint32
		switch scalar.(type) {
		case pgtype.Numeric:
			expected = pgtype.NumericOID
		case pgtype.UUID:
			expected = pgtype.UUIDOID
		case pgtype.Date:
			expected = pgtype.DateOID
		case pgtype.Timestamp:
			expected = pgtype.TimestampOID
		case pgtype.Timestamptz:
			expected = pgtype.TimestamptzOID
		case []byte:
			expected = pgtype.ByteaOID
		}
		if expected == oid {
			return scalar, nil
		}
	}
	return nil, errors.New("PostgreSQL array element does not match its declared scalar type")
}
