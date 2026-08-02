package typhon

import "fmt"

type compiler struct {
	module *Module
}

type functionCompiler struct {
	module   *Module
	fn       *Function
	isModule bool
}

func CompileModule(module *Module) (*Program, error) {
	c := &compiler{module: module}
	fn := &Function{
		Name:       "<module>",
		LocalIndex: map[string]int{},
		ModulePath: module.Path,
	}
	fc := &functionCompiler{module: module, fn: fn, isModule: true}
	for _, stmt := range module.Stmts {
		if err := fc.compileStmt(stmt); err != nil {
			return nil, err
		}
	}
	fc.emit(Instr{Op: OpConst, Val: NoneValue()})
	fc.emit(Instr{Op: OpReturn})
	_ = c
	return &Program{Module: fn}, nil
}

func compileFunction(module *Module, fnDef *FuncDefStmt) (*Function, error) {
	fn := &Function{
		Name:       fnDef.Name,
		LocalIndex: map[string]int{},
		ModulePath: module.Path,
	}
	fc := &functionCompiler{module: module, fn: fn}
	for _, param := range fnDef.Params {
		fc.ensureLocal(param.Name)
		fn.Params = append(fn.Params, param.Name)
	}
	fc.collectLocals(fnDef.Body)
	for _, stmt := range fnDef.Body {
		if err := fc.compileStmt(stmt); err != nil {
			return nil, err
		}
	}
	fc.emit(Instr{Op: OpConst, Val: NoneValue()})
	fc.emit(Instr{Op: OpReturn})
	return fn, nil
}

func (fc *functionCompiler) collectLocals(stmts []Stmt) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *AnnAssignStmt:
			if name, ok := s.Target.(*NameExpr); ok {
				fc.ensureLocal(name.Name)
			}
		case *ForStmt:
			targets := s.Targets
			if len(targets) == 0 {
				targets = []ForTarget{{Name: s.Name}}
			}
			for _, target := range targets {
				fc.ensureLocal(target.Name)
			}
			fc.collectLocals(s.Body)
		case *IfStmt:
			fc.collectLocals(s.Body)
		}
	}
}

func (fc *functionCompiler) ensureLocal(name string) int {
	if idx, ok := fc.fn.LocalIndex[name]; ok {
		return idx
	}
	idx := len(fc.fn.LocalNames)
	fc.fn.LocalIndex[name] = idx
	fc.fn.LocalNames = append(fc.fn.LocalNames, name)
	return idx
}

func (fc *functionCompiler) emit(instr Instr) int {
	fc.fn.Code = append(fc.fn.Code, instr)
	return len(fc.fn.Code) - 1
}

func (fc *functionCompiler) patch(pos int, target int) {
	fc.fn.Code[pos].A = target
}

func (fc *functionCompiler) compileStmt(stmt Stmt) error {
	switch s := stmt.(type) {
	case *TypeAliasStmt:
		return nil
	case *ImportFromStmt:
		fc.emit(Instr{Op: OpImportFrom, S: s.Module, Names: s.Names})
	case *ImportStmt:
		alias := s.Alias
		if alias == "" {
			alias = importBindingName(s.Name)
		}
		fc.emit(Instr{Op: OpImportModule, S: s.Name, Names: []string{alias}})
	case *FuncDefStmt:
		fn, err := compileFunction(fc.module, s)
		if err != nil {
			return err
		}
		fc.emit(Instr{Op: OpDefineFunc, S: s.Name, Fn: fn})
	case *ClassDefStmt:
		spec := &ClassSpec{Name: s.Name, Methods: map[string]*Function{}}
		for _, field := range s.Fields {
			spec.Fields = append(spec.Fields, field.Name)
		}
		for _, method := range s.Methods {
			fn, err := compileFunction(fc.module, method)
			if err != nil {
				return err
			}
			spec.Methods[method.Name] = fn
		}
		fc.emit(Instr{Op: OpDefineClass, Class: spec})
	case *AnnAssignStmt:
		if s.Value == nil {
			return nil
		}
		if attr, ok := s.Target.(*AttrExpr); ok {
			if err := fc.compileExpr(attr.Object); err != nil {
				return err
			}
			if err := fc.compileExpr(s.Value); err != nil {
				return err
			}
			fc.emit(Instr{Op: OpSetAttr, S: attr.Name})
			return nil
		}
		if err := fc.compileExpr(s.Value); err != nil {
			return err
		}
		return fc.compileStore(s.Target)
	case *AssignStmt:
		if attr, ok := s.Target.(*AttrExpr); ok {
			if err := fc.compileExpr(attr.Object); err != nil {
				return err
			}
			if err := fc.compileExpr(s.Value); err != nil {
				return err
			}
			fc.emit(Instr{Op: OpSetAttr, S: attr.Name})
			return nil
		}
		if err := fc.compileExpr(s.Value); err != nil {
			return err
		}
		return fc.compileStore(s.Target)
	case *ReturnStmt:
		if s.Value == nil {
			fc.emit(Instr{Op: OpConst, Val: NoneValue()})
		} else if err := fc.compileExpr(s.Value); err != nil {
			return err
		}
		fc.emit(Instr{Op: OpReturn})
	case *IfStmt:
		if err := fc.compileExpr(s.Cond); err != nil {
			return err
		}
		jumpFalse := fc.emit(Instr{Op: OpJumpIfFalse})
		for _, child := range s.Body {
			if err := fc.compileStmt(child); err != nil {
				return err
			}
		}
		fc.patch(jumpFalse, len(fc.fn.Code))
	case *ForStmt:
		if err := fc.compileExpr(s.Iterable); err != nil {
			return err
		}
		fc.emit(Instr{Op: OpIter})
		start := len(fc.fn.Code)
		iterNext := fc.emit(Instr{Op: OpIterNext})
		targets := s.Targets
		if len(targets) == 0 {
			targets = []ForTarget{{Name: s.Name}}
		}
		if len(targets) > 1 {
			names := make([]string, 0, len(targets))
			for _, target := range targets {
				names = append(names, target.Name)
			}
			fc.emit(Instr{Op: OpUnpackStore, Names: names})
		} else if idx, ok := fc.fn.LocalIndex[targets[0].Name]; ok && !fc.isModule {
			fc.emit(Instr{Op: OpStoreLocal, A: idx})
		} else {
			fc.emit(Instr{Op: OpStoreGlobal, S: targets[0].Name})
		}
		for _, child := range s.Body {
			if err := fc.compileStmt(child); err != nil {
				return err
			}
		}
		fc.emit(Instr{Op: OpJump, A: start})
		fc.patch(iterNext, len(fc.fn.Code))
		fc.emit(Instr{Op: OpPop})
	case *ExprStmt:
		if err := fc.compileExpr(s.Expr); err != nil {
			return err
		}
		fc.emit(Instr{Op: OpPop})
	}
	return nil
}

func (fc *functionCompiler) compileStore(target Expr) error {
	switch t := target.(type) {
	case *NameExpr:
		if idx, ok := fc.fn.LocalIndex[t.Name]; ok && !fc.isModule {
			fc.emit(Instr{Op: OpStoreLocal, A: idx})
		} else {
			fc.emit(Instr{Op: OpStoreGlobal, S: t.Name})
		}
	case *AttrExpr:
		if err := fc.compileExpr(t.Object); err != nil {
			return err
		}
		fc.emit(Instr{Op: OpSetAttr, S: t.Name})
	default:
		return fmt.Errorf("%s:%d: unsupported assignment target", fc.module.Path, target.LineNo())
	}
	return nil
}

func (fc *functionCompiler) compileExpr(expr Expr) error {
	switch e := expr.(type) {
	case *NameExpr:
		if idx, ok := fc.fn.LocalIndex[e.Name]; ok && !fc.isModule {
			fc.emit(Instr{Op: OpLoadLocal, A: idx})
		} else {
			fc.emit(Instr{Op: OpLoadGlobal, S: e.Name})
		}
	case *IntExpr:
		fc.emit(Instr{Op: OpConst, Val: IntValue(e.Value)})
	case *FloatExpr:
		fc.emit(Instr{Op: OpConst, Val: FloatValue(e.Value)})
	case *StringExpr:
		fc.emit(Instr{Op: OpConst, Val: StringValue(e.Value)})
	case *BoolExpr:
		fc.emit(Instr{Op: OpConst, Val: BoolValue(e.Value)})
	case *NoneExpr:
		fc.emit(Instr{Op: OpConst, Val: NoneValue()})
	case *ListExpr:
		for _, item := range e.Elements {
			if err := fc.compileExpr(item); err != nil {
				return err
			}
		}
		fc.emit(Instr{Op: OpMakeList, A: len(e.Elements)})
	case *BinaryExpr:
		if err := fc.compileExpr(e.Left); err != nil {
			return err
		}
		if err := fc.compileExpr(e.Right); err != nil {
			return err
		}
		switch e.Op {
		case "+":
			fc.emit(Instr{Op: OpAdd})
		case "-":
			fc.emit(Instr{Op: OpSub})
		case "*":
			fc.emit(Instr{Op: OpMul})
		case "==":
			fc.emit(Instr{Op: OpEqual})
		case "!=":
			fc.emit(Instr{Op: OpNotEqual})
		case "is":
			fc.emit(Instr{Op: OpIs})
		case "is not":
			fc.emit(Instr{Op: OpIsNot})
		default:
			return fmt.Errorf("%s:%d: unsupported binary operator %q", fc.module.Path, e.Line, e.Op)
		}
	case *AttrExpr:
		if err := fc.compileExpr(e.Object); err != nil {
			return err
		}
		fc.emit(Instr{Op: OpGetAttr, S: e.Name})
	case *CallExpr:
		if err := fc.compileExpr(e.Callee); err != nil {
			return err
		}
		for _, arg := range e.Args {
			if err := fc.compileExpr(arg); err != nil {
				return err
			}
		}
		keywords := make([]string, 0, len(e.Keywords))
		for _, keyword := range e.Keywords {
			if err := fc.compileExpr(keyword.Value); err != nil {
				return err
			}
			keywords = append(keywords, keyword.Name)
		}
		fc.emit(Instr{Op: OpCall, A: len(e.Args) + len(e.Keywords), Keywords: keywords})
	case *FStringExpr:
		fc.emit(Instr{Op: OpConst, Val: StringValue("")})
		for _, part := range e.Parts {
			if part.Text != "" {
				fc.emit(Instr{Op: OpConst, Val: StringValue(part.Text)})
			} else {
				if err := fc.compileExpr(part.Expr); err != nil {
					return err
				}
				fc.emit(Instr{Op: OpString, S: part.Format})
			}
			fc.emit(Instr{Op: OpAdd})
		}
	default:
		return fmt.Errorf("%s:%d: unsupported expression", fc.module.Path, expr.LineNo())
	}
	return nil
}
