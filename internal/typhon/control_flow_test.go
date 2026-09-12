package typhon

import (
	"strings"
	"testing"
)

func TestControlFlowAndOperators(t *testing.T) {
	source := `
def sign(x: int) -> str:
    if x < 0:
        return "negative"
    elif x == 0:
        return "zero"
    else:
        return "positive"

def choose(b: bool) -> int:
    x: int
    if b:
        x = 1
    else:
        x = 2
    return x

def fail() -> bool:
    return 1 / 0 == 0

print(sign(-1), sign(0), sign(1), choose(False))
print(False and fail(), True or fail(), not 1 == 2)
print(-7 // 3, -7 % 3, 7 // -3, 7 % -3, 7 / 2, 2 * -3)
print(9007199254740993 > 9007199254740992.0, 1 == 1.0)
i: int = 0
total: int = 0
while i < 10:
    i = i + 1
    if i == 2:
        continue
    if i == 5:
        break
    for j: int in range(3):
        if j == 1:
            break
        total = total + i
print(total)
print(-9223372036854775808)
1 <= 2
2 >= 1
`
	output, err := captureRun(t, sourceFile(t, source))
	want := "negative zero positive 2\nFalse True True\n-3 2 -3 -2 3.5 -6\nTrue True\n8\n-9223372036854775808\n"
	if err != nil || output != want {
		t.Fatalf("got %q (%v), want %q", output, err, want)
	}
}

func TestControlFlowErrors(t *testing.T) {
	for _, source := range []string{"break\n", "continue\n", "else:\n    print(1)\n", "def f(b: bool) -> int:\n    x: int\n    while b:\n        x = 1\n    return x\n"} {
		if err := CheckFile(sourceFile(t, source)); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	for _, source := range []string{"print(1 / 0)\n", "print(1 // 0)\n", "print(1 % 0)\n"} {
		err := RunFile(sourceFile(t, source))
		if err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Fatal(err)
		}
	}
}

func TestConstructorInitialization(t *testing.T) {
	for _, source := range []string{
		"class A:\n    x: int\n",
		"class A:\n    x: int\n    def __init__(self) -> None:\n        print(self.x)\n        self.x = 1\n",
		"class A:\n    x: int\n    def __init__(self, b: bool) -> None:\n        if b:\n            self.x = 1\n",
		"class A:\n    x: int\n    def __init__(self) -> None:\n        return\n        self.x = 1\n",
	} {
		if err := CheckFile(sourceFile(t, source)); err == nil {
			t.Fatalf("accepted incomplete constructor: %s", source)
		}
	}
}
