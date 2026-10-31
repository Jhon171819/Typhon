package typhon

import "testing"

// Syntax errors are expected. A panic or a hang is always a regression.
func FuzzParseFile(f *testing.F) {
	f.Add("d: dict[str, int] = {'x': 1}\nd['x'] = 2\ndel d['x']\nprint('x' not in d)\n")
	for _, seed := range []string{"x: int = 1\n", "while True:\n    break\n", "def f(x: int) -> int:\n    return -x\n", "x: list[int] = [1, 2]\n", "print(f'{1 + 2}')\n", "if True:\n    print(1)\nelse:\n    print(2)\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 65536 {
			t.Skip()
		}
		_, _ = ParseFile("fuzz.ty", source)
	})
}
