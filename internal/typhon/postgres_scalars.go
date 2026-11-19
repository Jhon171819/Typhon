package typhon

import (
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func postgresScanTargets(fields []pgconn.FieldDescription) []any {
	targets := make([]any, len(fields))
	for i, field := range fields {
		switch field.DataTypeOID {
		case pgtype.ByteaOID:
			targets[i] = new([]byte)
		case pgtype.NumericOID:
			targets[i] = new(pgtype.Numeric)
		case pgtype.UUIDOID:
			targets[i] = new(pgtype.UUID)
		case pgtype.DateOID:
			targets[i] = new(pgtype.Date)
		case pgtype.TimestampOID:
			targets[i] = new(pgtype.Timestamp)
		case pgtype.TimestamptzOID:
			targets[i] = new(pgtype.Timestamptz)
		case pgtype.JSONOID, pgtype.JSONBOID:
			targets[i] = new(json.RawMessage)
		default:
			targets[i] = new(any)
		}
	}
	return targets
}

func postgresTypedValue(vm *VM, scanned any) (Value, error) {
	switch data := scanned.(type) {
	case *[]byte:
		if *data != nil {
			return nativeScalarValue(vm, "Bytes", *data)
		}
	case *pgtype.Numeric:
		if data.Valid {
			return nativeScalarValue(vm, "Decimal", *data)
		}
	case *pgtype.UUID:
		if data.Valid {
			return nativeScalarValue(vm, "UUID", *data)
		}
	case *pgtype.Date:
		if data.Valid {
			return nativeScalarValue(vm, "Date", *data)
		}
	case *pgtype.Timestamp:
		if data.Valid {
			return nativeScalarValue(vm, "LocalTimestamp", *data)
		}
	case *pgtype.Timestamptz:
		if data.Valid {
			return nativeScalarValue(vm, "Timestamp", *data)
		}
	case *json.RawMessage:
		if len(*data) > 0 {
			return loadJSON(vm, string(*data))
		}
	case *any:
		return postgresValue(*data, 0)
	default:
		return NoneValue(), errors.New("unsupported PostgreSQL scan target")
	}
	return NoneValue(), nil
}
