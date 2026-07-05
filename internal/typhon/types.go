package typhon

import "strings"

type TypeKind string

const (
	TypeAny    TypeKind = "any"
	TypeNone   TypeKind = "none"
	TypeInt    TypeKind = "int"
	TypeFloat  TypeKind = "float"
	TypeStr    TypeKind = "str"
	TypeBool   TypeKind = "bool"
	TypeList   TypeKind = "list"
	TypeClass  TypeKind = "class"
	TypeFunc   TypeKind = "func"
	TypeTask   TypeKind = "task"
	TypeUnion  TypeKind = "union"
	TypeModule TypeKind = "module"
	TypePy     TypeKind = "pyobject"
)

type Type struct {
	Kind    TypeKind
	Name    string
	Elem    *Type
	Options []Type
	Params  []Type
	Return  *Type
	Class   *ClassInfo
}

type ClassInfo struct {
	Name    string
	Fields  map[string]Type
	Order   []string
	Methods map[string]Type
}

type ModuleInfo struct {
	Path    string
	Exports map[string]Type
	Classes map[string]*ClassInfo
}

func AnyType() Type   { return Type{Kind: TypeAny, Name: "Any"} }
func NoneType() Type  { return Type{Kind: TypeNone, Name: "None"} }
func IntType() Type   { return Type{Kind: TypeInt, Name: "int"} }
func FloatType() Type { return Type{Kind: TypeFloat, Name: "float"} }
func StrType() Type   { return Type{Kind: TypeStr, Name: "str"} }
func BoolType() Type  { return Type{Kind: TypeBool, Name: "bool"} }
func TaskType() Type  { return Type{Kind: TypeTask, Name: "TyphonTask"} }
func PyType() Type    { return Type{Kind: TypePy, Name: "PyObject"} }

func (t Type) String() string {
	switch t.Kind {
	case TypeAny:
		return "Any"
	case TypeNone:
		return "None"
	case TypeInt, TypeFloat, TypeStr, TypeBool, TypeClass, TypeTask, TypeModule, TypePy:
		return t.Name
	case TypeList:
		if t.Elem == nil {
			return "list"
		}
		return "list[" + t.Elem.String() + "]"
	case TypeFunc:
		return "function"
	case TypeUnion:
		parts := make([]string, 0, len(t.Options))
		for _, option := range t.Options {
			parts = append(parts, option.String())
		}
		return strings.Join(parts, " | ")
	default:
		if t.Name != "" {
			return t.Name
		}
		return string(t.Kind)
	}
}

func (t Type) Equal(other Type) bool {
	if t.Kind != other.Kind {
		return false
	}
	switch t.Kind {
	case TypeList:
		if t.Elem == nil || other.Elem == nil {
			return t.Elem == nil && other.Elem == nil
		}
		return t.Elem.Equal(*other.Elem)
	case TypeClass:
		return t.Name == other.Name
	case TypeUnion:
		if len(t.Options) != len(other.Options) {
			return false
		}
		for i := range t.Options {
			if !t.Options[i].Equal(other.Options[i]) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func assignable(from Type, to Type) bool {
	if from.Equal(to) {
		return true
	}
	if to.Kind == TypeAny || from.Kind == TypeAny {
		return true
	}
	if from.Kind == TypeInt && to.Kind == TypeFloat {
		return true
	}
	if from.Kind == TypePy || to.Kind == TypePy {
		return true
	}
	if to.Kind == TypeUnion {
		for _, option := range to.Options {
			if assignable(from, option) {
				return true
			}
		}
		return false
	}
	if from.Kind == TypeUnion {
		for _, option := range from.Options {
			if !assignable(option, to) {
				return false
			}
		}
		return true
	}
	if to.Kind == TypeList && from.Kind == TypeList {
		if to.Elem == nil || from.Elem == nil {
			return true
		}
		return assignable(*from.Elem, *to.Elem)
	}
	return from.Equal(to)
}
