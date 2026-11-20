package typhon

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestNativeScalarJSONAndPrecision(t *testing.T) {
	path := sourceFile(t, `import json
import typhon.scalars as s
cash: s.Decimal = s.decimal('12345678901234567890.12345678901234567890')
larger: s.Decimal = s.decimal('12345678901234567890.12345678901234567891')
data: s.Bytes = s.bytes_hex('610062ff')
stamp: s.Timestamp = s.timestamp('2026-01-02T03:04:05.123456789-03:00')
same: s.Timestamp = s.timestamp('2026-01-02T08:04:05.123456789+02:00')
civil: s.LocalTimestamp = s.local_timestamp('2026-01-02T03:04:05')
print(cash.compare(larger), data.size, data.base64(), s.bytes_base64(data.base64()).hex())
print(stamp.text == same.text, civil.text, s.date('2024-02-29').text)
print(json.dumps([cash, data, stamp, s.uuid('A0B1C2D3-0000-4000-8000-000000000001')]))
try:
    data.utf8()
except ValidationError:
    print('invalid encoding')
`)
	output, err := captureRun(t, path)
	want := "-1 4 YQBi/w== 610062ff\nTrue 2026-01-02T03:04:05 2024-02-29\n" +
		`["12345678901234567890.12345678901234567890", "YQBi/w==", "2026-01-02T06:04:05.123456789Z", "a0b1c2d3-0000-4000-8000-000000000001"]` +
		"\ninvalid encoding\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
}

func TestNativeScalarInvalidValues(t *testing.T) {
	for _, expr := range []string{
		"s.bytes_hex('0')", "s.bytes_base64('not base64')", "s.uuid('secret-invalid')",
		"s.decimal('NaN')", "s.decimal('1e999')", "s.date('2025-02-29')", "s.date('0000-01-01')",
		"s.timestamp('2026-01-02T03:04:05')", "s.timestamp('2026-01-02T03:04:05.1234567891Z')",
		"s.timestamp('0001-01-01T00:00:00+01:00')", "s.local_timestamp('2026-01-02T03:04:05Z')",
	} {
		err := RunFile(sourceFile(t, "import typhon.scalars as s\n"+expr+"\n"))
		var ex *Exception
		if !errors.As(err, &ex) || ex.Kind() != validationKind || strings.Contains(err.Error(), "secret-invalid") {
			t.Errorf("%s: expected sanitized validation error, got %v", expr, err)
		}
	}
	if err := CheckFile(sourceFile(t, "import typhon.scalars as s\nx: s.Decimal = s.decimal(5)\n")); err == nil {
		t.Fatal("checker accepted a numeric argument in an exact decimal constructor")
	}
	engine := NewEngine()
	defer engine.Close()
	vm := &VM{engine: engine}
	value, err := parseNativeScalar(vm, "timestamp", "2026-01-02T03:04:05.000000001Z")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgresParams(vm, []Value{StringValue("SELECT $1"), ListValue([]Value{value})}); err == nil ||
		!strings.Contains(err.Error(), "microsecond precision") {
		t.Fatalf("SQL silently loses timestamp precision: %v", err)
	}
}

func TestNativePostgresTypedScalarsIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL for a dedicated test PostgreSQL")
	}
	t.Setenv("DATABASE_URL", dsn)
	output, err := captureRun(t, examplePath(t, "native_scalars.ty"))
	want := "0 610062\n" +
		`["a0b1c2d3-0000-4000-8000-000000000001", "12345678901234567890.12345678901234567890", "YQBi", "2026-01-02", "2026-01-02T06:04:05.123456Z", "2026-01-02T03:04:05.123456", {"id": 9007199254740993, "amount": "12345678901234567890.12345678901234567890"}]` +
		"\n"
	if err != nil || output != want {
		t.Fatalf("typed SQL round trip: %q (%v), want %q", output, err, want)
	}
}
