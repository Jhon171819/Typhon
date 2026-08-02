package typhon

import (
	"fmt"
	"strings"
)

type Op int

const (
	OpConst Op = iota
	OpLoadGlobal
	OpStoreGlobal
	OpLoadLocal
	OpStoreLocal
	OpPop
	OpAdd
	OpSub
	OpMul
	OpEqual
	OpNotEqual
	OpIs
	OpIsNot
	OpJumpIfFalse
	OpJump
	OpCall
	OpReturn
	OpMakeList
	OpGetAttr
	OpSetAttr
	OpIter
	OpIterNext
	OpDefineFunc
	OpDefineClass
	OpImportModule
	OpImportFrom
	OpString
	OpUnpackStore
)

func (op Op) String() string {
	switch op {
	case OpConst:
		return "CONST"
	case OpLoadGlobal:
		return "LOAD_GLOBAL"
	case OpStoreGlobal:
		return "STORE_GLOBAL"
	case OpLoadLocal:
		return "LOAD_LOCAL"
	case OpStoreLocal:
		return "STORE_LOCAL"
	case OpPop:
		return "POP"
	case OpAdd:
		return "ADD"
	case OpSub:
		return "SUB"
	case OpMul:
		return "MUL"
	case OpEqual:
		return "EQ"
	case OpNotEqual:
		return "NEQ"
	case OpIs:
		return "IS"
	case OpIsNot:
		return "IS_NOT"
	case OpJumpIfFalse:
		return "JUMP_IF_FALSE"
	case OpJump:
		return "JUMP"
	case OpCall:
		return "CALL"
	case OpReturn:
		return "RETURN"
	case OpMakeList:
		return "MAKE_LIST"
	case OpGetAttr:
		return "GET_ATTR"
	case OpSetAttr:
		return "SET_ATTR"
	case OpIter:
		return "ITER"
	case OpIterNext:
		return "ITER_NEXT"
	case OpDefineFunc:
		return "DEFINE_FUNC"
	case OpDefineClass:
		return "DEFINE_CLASS"
	case OpImportModule:
		return "IMPORT_MODULE"
	case OpImportFrom:
		return "IMPORT_FROM"
	case OpString:
		return "STRING"
	case OpUnpackStore:
		return "UNPACK_STORE"
	default:
		return fmt.Sprintf("OP_%d", int(op))
	}
}

type Instr struct {
	Op       Op
	A        int
	S        string
	Names    []string
	Keywords []string
	Val      Value
	Fn       *Function
	Class    *ClassSpec
}

type Function struct {
	Name       string
	Params     []string
	LocalNames []string
	LocalIndex map[string]int
	Code       []Instr
	ModulePath string
	Module     *LoadedModule
}

type ClassSpec struct {
	Name    string
	Fields  []string
	Methods map[string]*Function
}

type Program struct {
	Module *Function
}

func Disassemble(program *Program) string {
	var out strings.Builder
	writeFunction(&out, program.Module)
	for _, instr := range program.Module.Code {
		if instr.Op == OpDefineFunc && instr.Fn != nil {
			writeFunction(&out, instr.Fn)
		}
		if instr.Op == OpDefineClass && instr.Class != nil {
			fmt.Fprintf(&out, "\nclass %s fields=%v\n", instr.Class.Name, instr.Class.Fields)
			for _, method := range instr.Class.Methods {
				writeFunction(&out, method)
			}
		}
	}
	return out.String()
}

func writeFunction(out *strings.Builder, fn *Function) {
	fmt.Fprintf(out, "\nfunction %s locals=%v\n", fn.Name, fn.LocalNames)
	for i, instr := range fn.Code {
		fmt.Fprintf(out, "  %04d %-15s", i, instr.Op.String())
		switch instr.Op {
		case OpConst:
			fmt.Fprintf(out, " %s", instr.Val.String())
		case OpLoadGlobal, OpStoreGlobal, OpGetAttr, OpSetAttr, OpDefineFunc:
			fmt.Fprintf(out, " %s", instr.S)
		case OpLoadLocal, OpStoreLocal, OpJump, OpJumpIfFalse, OpCall, OpMakeList, OpIterNext:
			fmt.Fprintf(out, " %d", instr.A)
		case OpUnpackStore:
			fmt.Fprintf(out, " %v", instr.Names)
		case OpDefineClass:
			fmt.Fprintf(out, " %s", instr.Class.Name)
		case OpImportModule:
			fmt.Fprintf(out, " %s as %s", instr.S, instr.Names[0])
		case OpImportFrom:
			fmt.Fprintf(out, " %s %v", instr.S, instr.Names)
		}
		out.WriteByte('\n')
	}
}
