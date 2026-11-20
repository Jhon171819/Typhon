package typhon

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxNativeBytes  = 8 << 20
	maxDecimalText  = 256
	localTimeLayout = "2006-01-02T15:04:05.999999999"
)

var decimalPattern = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)
var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
var localTimePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?$`)

type nativeScalar struct {
	sql  any
	text string
}

func scalarType(kind string) Type {
	typ := Type{Kind: TypeNative, Name: "typhon.scalars." + kind, Members: map[string]Type{}}
	if kind == "Bytes" {
		typ.Members["size"] = IntType()
		for _, method := range []string{"utf8", "hex", "base64"} {
			typ.Members[method] = nativeFuncType("scalars.bytes."+method, nil, StrType(), 0)
		}
	} else {
		typ.Members["text"] = StrType()
		if kind == "Decimal" {
			decimal := Type{Kind: TypeNative, Name: typ.Name}
			typ.Members["compare"] = nativeFuncType("scalars.decimal.compare", []Type{decimal}, IntType(), 0)
		}
	}
	return typ
}

var scalarConstructors = [...]struct{ name, kind string }{
	{"bytes_utf8", "Bytes"}, {"bytes_hex", "Bytes"}, {"bytes_base64", "Bytes"},
	{"decimal", "Decimal"}, {"uuid", "UUID"}, {"date", "Date"},
	{"timestamp", "Timestamp"}, {"local_timestamp", "LocalTimestamp"},
}

func scalarTypes() map[string]Type {
	types := map[string]Type{}
	for _, constructor := range scalarConstructors {
		typ := scalarType(constructor.kind)
		types[constructor.kind] = typ
		types[constructor.name] = nativeFuncType("scalars."+constructor.name, []Type{StrType()}, typ, 0)
	}
	return types
}

func scalarExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	for _, constructor := range scalarConstructors {
		exports[constructor.name] = driverFunc(types[constructor.name], func(vm *VM, args []Value) (Value, error) {
			return parseNativeScalar(vm, constructor.name, args[0].Str)
		})
	}
	return exports
}

func parseNativeScalar(vm *VM, name, text string) (Value, error) {
	switch name {
	case "bytes_utf8", "bytes_hex", "bytes_base64":
		var data []byte
		var err error
		switch name {
		case "bytes_utf8":
			if len(text) > maxNativeBytes || !utf8.ValidString(text) {
				return NoneValue(), errors.New("expected UTF-8 text within the byte limit")
			}
			data = []byte(text)
		case "bytes_hex":
			if len(text) > maxNativeBytes*2 {
				return NoneValue(), errors.New("byte limit exceeded")
			}
			data, err = hex.DecodeString(text)
		case "bytes_base64":
			if len(text) > base64.StdEncoding.EncodedLen(maxNativeBytes) || strings.ContainsAny(text, "\r\n") {
				return NoneValue(), errors.New("expected standard base64 within the byte limit")
			}
			data, err = base64.StdEncoding.Strict().DecodeString(text)
		}
		if err != nil {
			return NoneValue(), errors.New("invalid binary encoding")
		}
		return nativeScalarValue(vm, "Bytes", data)
	case "decimal":
		var numeric pgtype.Numeric
		if len(text) > maxDecimalText || !decimalPattern.MatchString(text) || numeric.Scan(text) != nil {
			return NoneValue(), errors.New("expected a finite decimal with at most 256 characters")
		}
		return nativeScalarValue(vm, "Decimal", numeric)
	case "uuid":
		var id pgtype.UUID
		if len(text) != 36 || text[8] != '-' || text[13] != '-' || text[18] != '-' || text[23] != '-' || id.Scan(text) != nil {
			return NoneValue(), errors.New("expected a canonical UUID")
		}
		return nativeScalarValue(vm, "UUID", id)
	case "date":
		parsed, err := time.Parse(time.DateOnly, text)
		if err != nil {
			return NoneValue(), errors.New("expected an ISO 8601 date")
		}
		return nativeScalarValue(vm, "Date", pgtype.Date{Time: parsed, Valid: true})
	case "timestamp":
		if !timestampPattern.MatchString(text) {
			return NoneValue(), errors.New("expected an RFC3339 timestamp with a timezone and up to 9 fractional digits")
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return NoneValue(), errors.New("invalid RFC3339 timestamp")
		}
		return nativeScalarValue(vm, "Timestamp", pgtype.Timestamptz{Time: parsed.UTC(), Valid: true})
	case "local_timestamp":
		if !localTimePattern.MatchString(text) {
			return NoneValue(), errors.New("expected a local timestamp with up to 9 fractional digits")
		}
		parsed, err := time.Parse(localTimeLayout, text)
		if err != nil {
			return NoneValue(), errors.New("invalid local timestamp")
		}
		return nativeScalarValue(vm, "LocalTimestamp", pgtype.Timestamp{Time: parsed, Valid: true})
	}
	return NoneValue(), errors.New("unknown scalar constructor")
}

func nativeScalarValue(vm *VM, kind string, sql any) (Value, error) {
	typ := scalarType(kind)
	attrs := map[string]Value{}
	scalar := &nativeScalar{sql: sql}
	switch data := sql.(type) {
	case []byte:
		if len(data) > maxNativeBytes {
			return NoneValue(), errors.New("byte limit exceeded")
		}
		if err := vm.chargeBytes(int64(len(data))); err != nil {
			return NoneValue(), err
		}
		attrs["size"] = IntValue(int64(len(data)))
		attrs["hex"] = driverFunc(typ.Members["hex"], func(vm *VM, args []Value) (Value, error) {
			if err := vm.chargeBytes(int64(hex.EncodedLen(len(data)))); err != nil {
				return NoneValue(), err
			}
			return StringValue(hex.EncodeToString(data)), nil
		})
		attrs["base64"] = driverFunc(typ.Members["base64"], func(vm *VM, args []Value) (Value, error) {
			if err := vm.chargeBytes(int64(base64.StdEncoding.EncodedLen(len(data)))); err != nil {
				return NoneValue(), err
			}
			return StringValue(base64.StdEncoding.EncodeToString(data)), nil
		})
		attrs["utf8"] = driverFunc(typ.Members["utf8"], func(vm *VM, args []Value) (Value, error) {
			if !utf8.Valid(data) {
				return NoneValue(), errors.New("bytes are not valid UTF-8")
			}
			if err := vm.chargeBytes(int64(len(data))); err != nil {
				return NoneValue(), err
			}
			return StringValue(string(data)), nil
		})
	case pgtype.Numeric:
		if !data.Valid || data.NaN || data.InfinityModifier != pgtype.Finite || data.Int == nil ||
			data.Exp < -maxDecimalText || data.Exp > maxDecimalText || data.Int.BitLen() > maxDecimalText*4 {
			return NoneValue(), errors.New("decimal exceeds finite precision limits")
		}
		encoded, err := data.Value()
		if err != nil {
			return NoneValue(), errors.New("could not render decimal")
		}
		scalar.text = encoded.(string)
		if len(scalar.text) > maxDecimalText {
			return NoneValue(), errors.New("decimal exceeds finite precision limits")
		}
		attrs["compare"] = driverFunc(typ.Members["compare"], func(vm *VM, args []Value) (Value, error) {
			other, ok := scalarData(args[0])
			if !ok {
				return NoneValue(), errors.New("expected Decimal")
			}
			left, _ := new(big.Rat).SetString(scalar.text)
			right, _ := new(big.Rat).SetString(other.text)
			return IntValue(int64(left.Cmp(right))), nil
		})
	case pgtype.UUID:
		scalar.text = data.String()
	case pgtype.Date:
		if err := checkScalarYear(data.Time, data.InfinityModifier); err != nil {
			return NoneValue(), err
		}
		scalar.text = data.Time.Format(time.DateOnly)
	case pgtype.Timestamp:
		if err := checkScalarYear(data.Time, data.InfinityModifier); err != nil {
			return NoneValue(), err
		}
		scalar.text = data.Time.Format(localTimeLayout)
	case pgtype.Timestamptz:
		if err := checkScalarYear(data.Time.UTC(), data.InfinityModifier); err != nil {
			return NoneValue(), err
		}
		scalar.text = data.Time.UTC().Format(time.RFC3339Nano)
	default:
		return NoneValue(), errors.New("unsupported native scalar")
	}
	if kind != "Bytes" {
		attrs["text"] = StringValue(scalar.text)
	}
	value := nativeObject(vm, typ, attrs)
	value.NativeObject.data = scalar
	return value, nil
}

func checkScalarYear(value time.Time, modifier pgtype.InfinityModifier) error {
	if modifier != pgtype.Finite || value.Year() < 1 || value.Year() > 9999 {
		return errors.New("date or timestamp outside years 0001 to 9999")
	}
	return nil
}

func scalarData(value Value) (*nativeScalar, bool) {
	if value.Kind != ValueNativeObject || value.NativeObject == nil {
		return nil, false
	}
	data, ok := value.NativeObject.data.(*nativeScalar)
	return data, ok
}
