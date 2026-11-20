package typhon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkDictionaryArrayRead(b *testing.B) {
	for _, access := range []string{"row['id']", "row.get('id', 0)"} {
		b.Run(access, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "read.ty")
			source := fmt.Sprintf(`
def total(rows: list[dict[str, int]]) -> int:
    result: int = 0
    for row: dict[str, int] in rows:
        result = result + %s
    return result
`, access)
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				b.Fatal(err)
			}
			e := NewEngine()
			defer e.Close()
			m, err := e.LoadModule(path)
			if err != nil {
				b.Fatal(err)
			}
			if err := e.ExecuteModule(m, false); err != nil {
				b.Fatal(err)
			}
			items := make([]Value, 10000)
			for i := range items {
				items[i] = DictValue()
				for _, key := range []string{"id", "score", "count"} {
					if err := items[i].Dict.put(StringValue(key), IntValue(int64(i))); err != nil {
						b.Fatal(err)
					}
				}
			}
			args := []Value{ListValue(items)}
			run := func() {
				vm := &VM{engine: e, module: m}
				result, err := vm.callValue(m.Globals["total"], args, nil)
				if err != nil || result.Int != 49995000 {
					b.Fatalf("total = %v, error = %v", result, err)
				}
			}
			run() // Bind collection contracts before measuring repeated reads.
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				run()
			}
		})
	}
}

func BenchmarkDictionaryLookup(b *testing.B) {
	d := DictValue()
	key := StringValue("id")
	if err := d.Dict.put(key, IntValue(42)); err != nil {
		b.Fatal(err)
	}
	if _, err := validateValue(d, dictType(StrType(), IntType())); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, err := getIndex(d, key)
		if err != nil || value.Int != 42 {
			b.Fatalf("lookup = %v, error = %v", value, err)
		}
	}
}

func BenchmarkDictionaryArrayValidation(b *testing.B) {
	typ := listType(dictType(StrType(), unionType(IntType(), StrType())))
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				items := make([]Value, size)
				for j := range items {
					items[j] = DictValue()
					if err := items[j].Dict.put(StringValue("id"), IntValue(int64(j))); err != nil {
						b.Fatal(err)
					}
				}
				rows := ListValue(items)
				b.StartTimer()
				if _, err := validateValue(rows, typ); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
