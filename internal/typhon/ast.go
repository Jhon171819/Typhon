package typhon

import "fmt"

type Module struct {
	Path  string
	Dir   string
	Stmts []Stmt
}

type Stmt interface {
	stmtNode()
	LineNo() int
}

type Expr interface {
	exprNode()
	LineNo() int
}

type TypeRef struct {
	Name    string
	Elem    *TypeRef
	Options []TypeRef
	Literal string
}

func (t TypeRef) String() string {
	if len(t.Options) > 0 {
		out := ""
		for i, option := range t.Options {
			if i > 0 {
				out += " | "
			}
			out += option.String()
		}
		return out
	}
	if t.Elem != nil {
		return fmt.Sprintf("%s[%s]", t.Name, t.Elem.String())
	}
	if t.Literal != "" {
		return fmt.Sprintf("%q", t.Literal)
	}
	return t.Name
}

type Param struct {
	Name string
	Type TypeRef
	Line int
}

type TypeAliasStmt struct {
	Line  int
	Name  string
	Value TypeRef
}

func (*TypeAliasStmt) stmtNode()     {}
func (s *TypeAliasStmt) LineNo() int { return s.Line }

type ImportFromStmt struct {
	Line   int
	Module string
	Names  []string
}

func (*ImportFromStmt) stmtNode()     {}
func (s *ImportFromStmt) LineNo() int { return s.Line }

type ImportStmt struct {
	Line  int
	Name  string
	Alias string
}

func (*ImportStmt) stmtNode()     {}
func (s *ImportStmt) LineNo() int { return s.Line }

type FuncDefStmt struct {
	Line       int
	Name       string
	Params     []Param
	ReturnType TypeRef
	Body       []Stmt
}

func (*FuncDefStmt) stmtNode()     {}
func (s *FuncDefStmt) LineNo() int { return s.Line }

type ClassDefStmt struct {
	Line    int
	Name    string
	Fields  []FieldDef
	Methods []*FuncDefStmt
	Body    []Stmt
}

func (*ClassDefStmt) stmtNode()     {}
func (s *ClassDefStmt) LineNo() int { return s.Line }

type FieldDef struct {
	Line int
	Name string
	Type TypeRef
}

type AnnAssignStmt struct {
	Line   int
	Target Expr
	Type   TypeRef
	Value  Expr
}

func (*AnnAssignStmt) stmtNode()     {}
func (s *AnnAssignStmt) LineNo() int { return s.Line }

type AssignStmt struct {
	Line   int
	Target Expr
	Value  Expr
}

func (*AssignStmt) stmtNode()     {}
func (s *AssignStmt) LineNo() int { return s.Line }

type ReturnStmt struct {
	Line  int
	Value Expr
}

func (*ReturnStmt) stmtNode()     {}
func (s *ReturnStmt) LineNo() int { return s.Line }

type IfStmt struct {
	Line int
	Cond Expr
	Body []Stmt
}

func (*IfStmt) stmtNode()     {}
func (s *IfStmt) LineNo() int { return s.Line }

type ForStmt struct {
	Line     int
	Name     string
	ItemType TypeRef
	Targets  []ForTarget
	Iterable Expr
	Body     []Stmt
}

func (*ForStmt) stmtNode()     {}
func (s *ForStmt) LineNo() int { return s.Line }

type ForTarget struct {
	Name string
	Type TypeRef
	Line int
}

type ExprStmt struct {
	Line int
	Expr Expr
}

func (*ExprStmt) stmtNode()     {}
func (s *ExprStmt) LineNo() int { return s.Line }

type NameExpr struct {
	Line int
	Name string
}

func (*NameExpr) exprNode()     {}
func (e *NameExpr) LineNo() int { return e.Line }

type IntExpr struct {
	Line  int
	Value int64
}

func (*IntExpr) exprNode()     {}
func (e *IntExpr) LineNo() int { return e.Line }

type FloatExpr struct {
	Line  int
	Value float64
}

func (*FloatExpr) exprNode()     {}
func (e *FloatExpr) LineNo() int { return e.Line }

type StringExpr struct {
	Line  int
	Value string
}

func (*StringExpr) exprNode()     {}
func (e *StringExpr) LineNo() int { return e.Line }

type BoolExpr struct {
	Line  int
	Value bool
}

func (*BoolExpr) exprNode()     {}
func (e *BoolExpr) LineNo() int { return e.Line }

type NoneExpr struct {
	Line int
}

func (*NoneExpr) exprNode()     {}
func (e *NoneExpr) LineNo() int { return e.Line }

type ListExpr struct {
	Line     int
	Elements []Expr
}

func (*ListExpr) exprNode()     {}
func (e *ListExpr) LineNo() int { return e.Line }

type CallExpr struct {
	Line     int
	Callee   Expr
	Args     []Expr
	Keywords []KeywordArg
}

func (*CallExpr) exprNode()     {}
func (e *CallExpr) LineNo() int { return e.Line }

type KeywordArg struct {
	Name  string
	Value Expr
}

type AttrExpr struct {
	Line   int
	Object Expr
	Name   string
}

func (*AttrExpr) exprNode()     {}
func (e *AttrExpr) LineNo() int { return e.Line }

type BinaryExpr struct {
	Line  int
	Left  Expr
	Op    string
	Right Expr
}

func (*BinaryExpr) exprNode()     {}
func (e *BinaryExpr) LineNo() int { return e.Line }

type FStringExpr struct {
	Line  int
	Parts []FStringPart
}

func (*FStringExpr) exprNode()     {}
func (e *FStringExpr) LineNo() int { return e.Line }

type FStringPart struct {
	Text   string
	Expr   Expr
	Format string
}
