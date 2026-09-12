package typhon

import (
	"errors"
	"fmt"
	"strings"
)

type ImportResolver interface {
	ResolveImport(module string, fromDir string) (*ModuleInfo, error)
}

type checker struct {
	loopDepth int
	module    *Module
	resolver  ImportResolver
	aliases   map[string]Type
	globals   map[string]Type
	classes   map[string]*ClassInfo
	errors    []error
}

func CheckModule(module *Module, resolver ImportResolver) (*ModuleInfo, error) {
	module.Contracts = map[Expr]Type{}
	c := &checker{
		module:   module,
		resolver: resolver,
		aliases:  map[string]Type{},
		globals:  builtinTypes(),
		classes:  map[string]*ClassInfo{},
	}
	c.collect(module.Stmts)
	top := newScope(c.globals)
	top.module = true
	c.checkStatements(module.Stmts, top, nil, NoneType())
	if len(c.errors) > 0 {
		return nil, c.errors[0]
	}
	exports := map[string]Type{}
	for name, typ := range c.globals {
		if !isBuiltinName(name) {
			exports[name] = typ
		}
	}
	return &ModuleInfo{Path: module.Path, Exports: exports, Classes: c.classes}, nil
}

func builtinTypes() map[string]Type {
	return map[string]Type{
		"print":       {Kind: TypeFunc, Name: "print", Variadic: true, Params: []Type{AnyType()}, Return: typePtr(NoneType())},
		"spawn":       {Kind: TypeFunc, Name: "spawn", Variadic: true, Params: []Type{AnyType()}, Return: typePtr(TaskType())},
		"join":        {Kind: TypeFunc, Name: "join", Params: []Type{TaskType()}, Return: typePtr(AnyType())},
		"cancel":      {Kind: TypeFunc, Name: "cancel", Params: []Type{TaskType()}, Return: typePtr(NoneType())},
		"enumerate":   {Kind: TypeFunc, Name: "enumerate", Params: []Type{AnyType()}, Return: typePtr(Type{Kind: TypeList, Name: "list", Elem: typePtr(AnyType())})},
		"range":       {Kind: TypeFunc, Name: "range", Variadic: true, Params: []Type{AnyType()}, Return: typePtr(Type{Kind: TypeList, Name: "list", Elem: typePtr(IntType())})},
		"TyphonTask":  TaskType(),
		"PyObject":    PyType(),
		"int":         IntType(),
		"float":       FloatType(),
		"str":         StrType(),
		"bool":        BoolType(),
		"None":        NoneType(),
		"void":        NoneType(),
		"list":        {Kind: TypeList, Name: "list"},
		"__name__":    StrType(),
		"__file__":    StrType(),
		"__package__": AnyType(),
	}
}

func isBuiltinName(name string) bool {
	switch name {
	case "print", "spawn", "join", "cancel", "enumerate", "range", "TyphonTask", "PyObject", "int", "float", "str", "bool", "None", "void", "list", "__name__", "__file__", "__package__":
		return true
	default:
		return false
	}
}

func (c *checker) collect(stmts []Stmt) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *TypeAliasStmt:
			typ, err := c.resolveTypeRef(s.Value)
			if err != nil {
				c.fail(s.Line, err.Error())
				continue
			}
			c.aliases[s.Name] = typ
			c.globals[s.Name] = typ
		case *ImportFromStmt:
			c.collectImport(s)
		case *ImportStmt:
			alias := s.Alias
			if alias == "" {
				alias = importBindingName(s.Name)
			}
			c.globals[alias] = PyType()
		case *FuncDefStmt:
			c.globals[s.Name] = c.functionType(s, nil)
		case *ClassDefStmt:
			classInfo := &ClassInfo{Name: s.Name, Module: c.module.Path, Fields: map[string]Type{}, Methods: map[string]Type{}}
			s.Info = classInfo
			for _, field := range s.Fields {
				typ, err := c.resolveTypeRef(field.Type)
				if err != nil {
					c.fail(field.Line, err.Error())
					continue
				}
				classInfo.Fields[field.Name] = typ
				classInfo.Order = append(classInfo.Order, field.Name)
			}
			c.classes[s.Name] = classInfo
			classType := Type{Kind: TypeClass, Name: s.Name, Class: classInfo}
			c.globals[s.Name] = classType
			for _, method := range s.Methods {
				classInfo.Methods[method.Name] = c.functionType(method, classInfo)
			}
		}
	}
}

func (c *checker) collectImport(stmt *ImportFromStmt) {
	if stmt.Module == "typhon.parallel" {
		for _, name := range stmt.Names {
			if typ, ok := c.globals[name]; ok {
				c.globals[name] = typ
			} else {
				c.fail(stmt.Line, fmt.Sprintf("unknown typhon.parallel import %q", name))
			}
		}
		return
	}
	if isPythonImport(stmt.Module) {
		for _, name := range stmt.Names {
			c.globals[name] = PyType()
		}
		return
	}
	if c.resolver == nil {
		c.fail(stmt.Line, "imports are not available")
		return
	}
	info, err := c.resolver.ResolveImport(stmt.Module, c.module.Dir)
	if err != nil {
		if !errors.Is(err, ErrModuleNotFound) {
			c.fail(stmt.Line, err.Error())
			return
		}
		for _, name := range stmt.Names {
			c.globals[name] = PyType()
		}
		return
	}
	for _, name := range stmt.Names {
		typ, ok := info.Exports[name]
		if !ok {
			c.fail(stmt.Line, fmt.Sprintf("module %q has no export %q", stmt.Module, name))
			continue
		}
		c.globals[name] = typ
		if typ.Kind == TypeClass && typ.Class != nil {
			c.classes[name] = typ.Class
		}
	}
}

func (c *checker) functionType(fn *FuncDefStmt, classInfo *ClassInfo) Type {
	params := make([]Type, 0, len(fn.Params))
	for i, param := range fn.Params {
		if (param.Name == "self" || param.Name == "cls") && i == 0 && classInfo != nil {
			params = append(params, Type{Kind: TypeClass, Name: classInfo.Name, Class: classInfo})
			continue
		}
		typ, err := c.resolveTypeRef(param.Type)
		if err != nil {
			c.fail(param.Line, err.Error())
			typ = AnyType()
		}
		params = append(params, typ)
	}
	ret, err := c.resolveTypeRef(fn.ReturnType)
	if err != nil {
		c.fail(fn.Line, err.Error())
		ret = AnyType()
	}
	fn.CheckedType = Type{Kind: TypeFunc, Name: fn.Name, Params: params, Return: typePtr(ret)}
	return fn.CheckedType
}

func (c *checker) checkStatements(stmts []Stmt, scope *scope, classInfo *ClassInfo, expectedReturn Type) {
	for _, stmt := range stmts {
		c.checkStatement(stmt, scope, classInfo, expectedReturn)
	}
}

func (c *checker) checkStatement(stmt Stmt, scope *scope, classInfo *ClassInfo, expectedReturn Type) {
	switch s := stmt.(type) {
	case *TypeAliasStmt, *ImportFromStmt, *ImportStmt:
		return
	case *FuncDefStmt:
		c.checkFunction(s, classInfo)
	case *ClassDefStmt:
		c.checkClass(s)
	case *AnnAssignStmt:
		c.checkAnnAssign(s, scope, classInfo)
	case *AssignStmt:
		c.checkAssign(s, scope, classInfo)
	case *ReturnStmt:
		c.checkInitializedFields(s.Line, scope)
		var actual Type
		if s.Value == nil {
			actual = NoneType()
		} else {
			actual = c.inferExpected(s.Value, expectedReturn, scope, classInfo)
		}
		if !assignable(actual, expectedReturn) {
			c.fail(s.Line, fmt.Sprintf("return expected %s, got %s", expectedReturn.String(), actual.String()))
		}
	case *IfStmt:
		c.inferExpr(s.Cond, scope, classInfo)
		child := scope.child()
		narrowNone(s.Cond, child, true)
		c.checkStatements(s.Body, child, classInfo, expectedReturn)
		other := scope.child()
		narrowNone(s.Cond, other, false)
		c.checkStatements(s.Else, other, classInfo, expectedReturn)
		for name := range scope.initialized {
			if alwaysReturns(s.Body) {
				scope.initialized[name] = other.initialized[name]
			} else if alwaysReturns(s.Else) {
				scope.initialized[name] = child.initialized[name]
			} else {
				scope.initialized[name] = child.initialized[name] && other.initialized[name]
			}
			if alwaysReturns(s.Body) {
				if typ, ok := other.lookup(name); ok {
					scope.values[name] = typ
				}
			} else if alwaysReturns(s.Else) {
				if typ, ok := child.lookup(name); ok {
					scope.values[name] = typ
				}
			}
		}
	case *WhileStmt:
		c.inferExpr(s.Cond, scope, classInfo)
		c.loopDepth++
		c.checkStatements(s.Body, scope.child(), classInfo, expectedReturn)
		c.loopDepth--
	case *BreakStmt, *ContinueStmt:
		if c.loopDepth == 0 {
			c.fail(stmt.LineNo(), "break/continue requires a loop")
		}
	case *ForStmt:
		s.CheckedTypes = nil
		iterType := c.inferExpr(s.Iterable, scope, classInfo)
		child := scope.child()
		targets := s.Targets
		if len(targets) == 0 {
			targets = []ForTarget{{Name: s.Name, Type: s.ItemType, Line: s.Line}}
		}
		for _, target := range targets {
			itemType, err := c.resolveTypeRef(target.Type)
			if err != nil {
				c.fail(target.Line, err.Error())
				itemType = AnyType()
			}
			if len(targets) == 1 && iterType.Kind == TypeList && iterType.Elem != nil && !assignable(*iterType.Elem, itemType) {
				c.fail(s.Line, fmt.Sprintf("for variable expected %s, got %s", itemType.String(), iterType.Elem.String()))
			}
			child.set(target.Name, itemType)
			s.CheckedTypes = append(s.CheckedTypes, itemType)
		}
		c.loopDepth++
		c.checkStatements(s.Body, child, classInfo, expectedReturn)
		c.loopDepth--
	case *ExprStmt:
		if classInfo != nil {
			if name, ok := s.Expr.(*NameExpr); ok {
				c.fail(s.Line, fmt.Sprintf("class field '%s' must have an explicit type", name.Name))
				return
			}
		}
		c.inferExpr(s.Expr, scope, classInfo)
	}
}

func (c *checker) checkFunction(fn *FuncDefStmt, classInfo *ClassInfo) {
	previousLoopDepth := c.loopDepth
	c.loopDepth = 0
	defer func() { c.loopDepth = previousLoopDepth }()
	fnType := c.functionType(fn, classInfo)
	ret := NoneType()
	if fnType.Return != nil {
		ret = *fnType.Return
	}
	child := newScope(c.globals)
	c.declareLocals(fn.Body, child)
	if classInfo != nil && fn.Name == "__init__" {
		for field := range classInfo.Fields {
			child.initialized["self."+field] = false
		}
	}
	for i, param := range fn.Params {
		if (param.Name == "self" || param.Name == "cls") && i == 0 && classInfo != nil {
			child.set(param.Name, Type{Kind: TypeClass, Name: classInfo.Name, Class: classInfo})
			continue
		}
		typ, err := c.resolveTypeRef(param.Type)
		if err != nil {
			c.fail(param.Line, err.Error())
			typ = AnyType()
		}
		child.set(param.Name, typ)
	}
	c.checkStatements(fn.Body, child, classInfo, ret)
	if !alwaysReturns(fn.Body) {
		c.checkInitializedFields(fn.Line, child)
	}
	if !assignable(NoneType(), ret) && !alwaysReturns(fn.Body) {
		c.fail(fn.Line, fmt.Sprintf("%s must return %s on every path", fn.Name, ret))
	}
}

func (c *checker) checkClass(classDef *ClassDefStmt) {
	classInfo := c.classes[classDef.Name]
	if len(classInfo.Fields) > 0 {
		if _, ok := classInfo.Methods["__init__"]; !ok {
			c.fail(classDef.Line, "class with fields requires __init__ to initialize every field")
		}
	}
	for _, stmt := range classDef.Body {
		switch s := stmt.(type) {
		case *AnnAssignStmt:
			if _, ok := s.Target.(*NameExpr); !ok || s.Value != nil {
				c.fail(s.Line, "class fields must use annotated assignments")
			}
		case *FuncDefStmt:
			c.checkFunction(s, classInfo)
		case *ExprStmt:
			if name, ok := s.Expr.(*NameExpr); ok {
				c.fail(s.Line, fmt.Sprintf("class field '%s' must have an explicit type", name.Name))
			}
		default:
			c.fail(s.LineNo(), "unsupported statement in class body")
		}
	}
}

func (c *checker) checkAnnAssign(stmt *AnnAssignStmt, scope *scope, classInfo *ClassInfo) {
	typ, err := c.resolveTypeRef(stmt.Type)
	if err != nil {
		c.fail(stmt.Line, err.Error())
		typ = AnyType()
	}
	if typ.Kind == TypeList && typ.Elem == nil {
		c.fail(stmt.Line, "collection 'list' must include element types")
	}
	if stmt.Value != nil {
		actual := c.inferExpected(stmt.Value, typ, scope, classInfo)
		if !assignable(actual, typ) {
			c.fail(stmt.Line, fmt.Sprintf("assignment expected %s, got %s", typ.String(), actual.String()))
		}
	}
	switch target := stmt.Target.(type) {
	case *NameExpr:
		if previous, exists := scope.lookup(target.Name); exists {
			if _, declared := scope.initialized[target.Name]; declared && !previous.Equal(typ) {
				c.fail(stmt.Line, fmt.Sprintf("cannot redeclare %s from %s to %s", target.Name, previous, typ))
			}
		}
		scope.set(target.Name, typ)
		scope.initialized[target.Name] = stmt.Value != nil
		c.module.Contracts[target] = typ
		if scope.module {
			scope.globals[target.Name] = typ
		}
	case *AttrExpr:
		c.module.Contracts[target] = typ
		c.checkAttrAssign(stmt.Line, target, typ, scope, classInfo)
		if name, ok := target.Object.(*NameExpr); ok && name.Name == "self" && stmt.Value != nil {
			scope.initialized["self."+target.Name] = true
		}
	default:
		c.fail(stmt.Line, "unsupported annotated assignment target")
	}
}

func (c *checker) checkAssign(stmt *AssignStmt, scope *scope, classInfo *ClassInfo) {
	actual := c.inferExpr(stmt.Value, scope, classInfo)
	switch target := stmt.Target.(type) {
	case *NameExpr:
		expected, ok := scope.lookup(target.Name)
		if !ok {
			c.fail(stmt.Line, "variable declarations must use an explicit type")
			return
		}
		if !assignable(actual, expected) {
			c.fail(stmt.Line, fmt.Sprintf("assignment expected %s, got %s", expected.String(), actual.String()))
		}
		scope.initialized[target.Name] = true
		c.module.Contracts[target] = expected
	case *AttrExpr:
		objType := c.inferExpr(target.Object, scope, classInfo)
		if objType.Kind == TypeClass && objType.Class != nil {
			expected, ok := objType.Class.Fields[target.Name]
			if !ok {
				c.fail(stmt.Line, fmt.Sprintf("%s has no field %q", objType.Name, target.Name))
				return
			}
			c.module.Contracts[target] = expected
			if name, ok := target.Object.(*NameExpr); ok && name.Name == "self" {
				scope.initialized["self."+target.Name] = true
			}
			if ok && !assignable(actual, expected) {
				c.fail(stmt.Line, fmt.Sprintf("%s.%s expected %s, got %s", objType.Name, target.Name, expected.String(), actual.String()))
			}
			return
		}
		c.fail(stmt.Line, "unsupported attribute assignment target")
	default:
		c.fail(stmt.Line, "unsupported assignment target")
	}
}

func (c *checker) checkAttrAssign(line int, target *AttrExpr, typ Type, scope *scope, classInfo *ClassInfo) {
	objType := c.inferExpr(target.Object, scope, classInfo)
	if objType.Kind == TypeClass && objType.Class != nil {
		expected, ok := objType.Class.Fields[target.Name]
		if !ok {
			c.fail(line, fmt.Sprintf("%s has no field %q", objType.Name, target.Name))
			return
		}
		if ok && !assignable(typ, expected) {
			c.fail(line, fmt.Sprintf("%s.%s expected %s, got %s", objType.Name, target.Name, expected.String(), typ.String()))
		}
		return
	}
	c.fail(line, "unsupported attribute assignment target")
}

func (c *checker) inferExpr(expr Expr, scope *scope, classInfo *ClassInfo) Type {
	switch e := expr.(type) {
	case *NameExpr:
		if typ, ok := scope.lookup(e.Name); ok {
			if initialized, local := scope.initialized[e.Name]; local && !initialized {
				c.fail(e.Line, fmt.Sprintf("variable %q is not initialized", e.Name))
			}
			return typ
		}
		if typ, ok := c.globals[e.Name]; ok {
			return typ
		}
		c.fail(e.Line, fmt.Sprintf("unknown name %q", e.Name))
		return AnyType()
	case *IntExpr:
		return IntType()
	case *FloatExpr:
		return FloatType()
	case *StringExpr:
		return Type{Kind: TypeLiteral, Name: e.Value}
	case *FStringExpr:
		for _, part := range e.Parts {
			if part.Expr != nil {
				typ := c.inferExpr(part.Expr, scope, classInfo)
				if part.Format != "" && part.Format != ".2f" {
					c.fail(e.Line, "unsupported f-string format")
				}
				if part.Format == ".2f" && typ.Kind != TypeInt && typ.Kind != TypeFloat && typ.Kind != TypeAny && typ.Kind != TypePy {
					c.fail(e.Line, ".2f format requires a number")
				}
			}
		}
		return StrType()
	case *BoolExpr:
		return BoolType()
	case *NoneExpr:
		return NoneType()
	case *ListExpr:
		if len(e.Elements) == 0 {
			return Type{Kind: TypeList, Name: "list", Elem: typePtr(Type{Kind: TypeNever})}
		}
		elem := c.inferExpr(e.Elements[0], scope, classInfo)
		if elem.Kind == TypeLiteral {
			elem = StrType()
		}
		for _, item := range e.Elements[1:] {
			itemType := c.inferExpr(item, scope, classInfo)
			if itemType.Kind == TypeLiteral {
				itemType = StrType()
			}
			elem = unionType(elem, itemType)
		}
		return Type{Kind: TypeList, Name: "list", Elem: typePtr(elem)}
	case *BinaryExpr:
		left := c.inferExpr(e.Left, scope, classInfo)
		rightScope := scope
		if e.Op == "and" || e.Op == "or" {
			rightScope = scope.child()
			narrowNone(e.Left, rightScope, e.Op == "and")
		}
		right := c.inferExpr(e.Right, rightScope, classInfo)
		switch e.Op {
		case "and", "or":
			return unionType(left, right)
		case "<", "<=", ">", ">=":
			_, err := binaryType("compare", left, right)
			if err != nil {
				c.fail(e.Line, err.Error())
			}
			return BoolType()
		case "==", "!=", "is", "is not":
			return BoolType()
		case "+", "-", "*", "/", "//", "%":
			result, err := binaryType(e.Op, left, right)
			if err != nil {
				c.fail(e.Line, err.Error())
			}
			return result
		default:
			return AnyType()
		}
	case *AttrExpr:
		return c.inferAttr(e, scope, classInfo)
	case *UnaryExpr:
		typ := c.inferExpr(e.Operand, scope, classInfo)
		if e.Op == "not" {
			return BoolType()
		}
		if typ.Kind != TypeInt && typ.Kind != TypeFloat && typ.Kind != TypeAny && typ.Kind != TypePy {
			c.fail(e.Line, "unary sign requires a number")
		}
		return typ
	case *CallExpr:
		return c.inferCall(e, scope, classInfo)
	default:
		return AnyType()
	}
}

func (c *checker) inferAttr(expr *AttrExpr, scope *scope, classInfo *ClassInfo) Type {
	objType := c.inferExpr(expr.Object, scope, classInfo)
	if objType.Kind == TypeClass && objType.Class != nil {
		if field, ok := objType.Class.Fields[expr.Name]; ok {
			if name, ok := expr.Object.(*NameExpr); ok && name.Name == "self" {
				if initialized, exists := scope.initialized["self."+expr.Name]; exists && !initialized {
					c.fail(expr.Line, fmt.Sprintf("field self.%s is not initialized", expr.Name))
				}
			}
			return field
		}
		if method, ok := objType.Class.Methods[expr.Name]; ok {
			if method.Kind == TypeFunc && len(method.Params) > 0 {
				method.Params = method.Params[1:]
			}
			return method
		}
		c.fail(expr.Line, fmt.Sprintf("%s has no attribute %q", objType.Name, expr.Name))
		return AnyType()
	}
	if objType.Kind == TypeStr || objType.Kind == TypeLiteral {
		switch expr.Name {
		case "strip", "title", "upper":
			return Type{Kind: TypeFunc, Name: expr.Name, Return: typePtr(StrType())}
		}
	}
	if objType.Kind == TypeList {
		if expr.Name == "append" {
			param := AnyType()
			if objType.Elem != nil {
				param = *objType.Elem
			}
			return Type{Kind: TypeFunc, Name: "append", Params: []Type{param}, Return: typePtr(NoneType())}
		}
	}
	if objType.Kind == TypePy {
		return PyType()
	}
	if objType.Kind != TypeAny {
		c.fail(expr.Line, fmt.Sprintf("%s has no attribute %q", objType, expr.Name))
	}
	return AnyType()
}

func (c *checker) checkInitializedFields(line int, scope *scope) {
	for name, initialized := range scope.initialized {
		if strings.HasPrefix(name, "self.") && !initialized {
			c.fail(line, fmt.Sprintf("field %s is not initialized on every constructor path", name))
		}
	}
}

func (c *checker) inferCall(expr *CallExpr, scope *scope, classInfo *ClassInfo) Type {
	if len(expr.Keywords) > 0 {
		callee := c.inferExpr(expr.Callee, scope, classInfo)
		if callee.Kind != TypePy && callee.Kind != TypeAny {
			c.fail(expr.Line, "Typhon calls do not accept keyword arguments")
		}
	}
	if name, ok := expr.Callee.(*NameExpr); ok {
		if class, ok := c.classes[name.Name]; ok {
			if initType, ok := class.Methods["__init__"]; ok {
				c.checkCallArgs(expr.Line, initType.Params[1:], expr.Args, scope, classInfo)
			} else if len(expr.Args) != 0 {
				c.fail(expr.Line, fmt.Sprintf("%s() expected 0 arguments, got %d", name.Name, len(expr.Args)))
			}
			return Type{Kind: TypeClass, Name: name.Name, Class: class}
		}
	}
	calleeType := c.inferExpr(expr.Callee, scope, classInfo)
	if calleeType.Kind == TypeAny || calleeType.Kind == TypePy {
		for _, arg := range expr.Args {
			c.inferExpr(arg, scope, classInfo)
		}
		for _, keyword := range expr.Keywords {
			c.inferExpr(keyword.Value, scope, classInfo)
		}
		return AnyType()
	}
	if calleeType.Kind != TypeFunc {
		c.fail(expr.Line, fmt.Sprintf("%s is not callable", calleeType.String()))
		return AnyType()
	}
	if calleeType.Variadic {
		for _, arg := range expr.Args {
			c.inferExpr(arg, scope, classInfo)
		}
		if calleeType.Name == "spawn" {
			if len(expr.Args) == 0 {
				c.fail(expr.Line, "spawn expected a function")
			} else {
				fn := c.inferExpr(expr.Args[0], scope, classInfo)
				if fn.Kind == TypeFunc && !fn.Variadic {
					c.checkCallArgs(expr.Line, fn.Params, expr.Args[1:], scope, classInfo)
				}
			}
		}
	} else {
		params := calleeType.Params
		if calleeType.Name == "join" && len(expr.Args) == 2 {
			params = []Type{TaskType(), FloatType()}
		}
		c.checkCallArgs(expr.Line, params, expr.Args, scope, classInfo)
	}
	if calleeType.Return == nil {
		return NoneType()
	}
	return *calleeType.Return
}

func (c *checker) checkCallArgs(line int, params []Type, args []Expr, scope *scope, classInfo *ClassInfo) {
	if len(args) != len(params) {
		c.fail(line, fmt.Sprintf("call expected %d arguments, got %d", len(params), len(args)))
		return
	}
	for i, arg := range args {
		actual := c.inferExpected(arg, params[i], scope, classInfo)
		if !assignable(actual, params[i]) {
			if actual.Kind == TypeLiteral {
				actual = StrType()
			}
			c.fail(line, fmt.Sprintf("argument %d expected %s, got %s", i+1, params[i].String(), actual.String()))
		}
	}
}

func narrowNone(expr Expr, scope *scope, positive bool) {
	binary, ok := expr.(*BinaryExpr)
	if !ok {
		return
	}
	if binary.Op == "and" && positive || binary.Op == "or" && !positive {
		narrowNone(binary.Left, scope, positive)
		narrowNone(binary.Right, scope, positive)
		return
	}
	name, ok := binary.Left.(*NameExpr)
	if !ok {
		return
	}
	if _, ok := binary.Right.(*NoneExpr); !ok {
		return
	}
	if binary.Op != "is" && binary.Op != "is not" {
		return
	}
	typ, ok := scope.lookup(name.Name)
	if !ok || typ.Kind != TypeUnion {
		return
	}
	wantNone := (binary.Op == "is") == positive
	var options []Type
	for _, option := range typ.Options {
		if (option.Kind == TypeNone) == wantNone {
			options = append(options, option)
		}
	}
	if len(options) > 0 {
		scope.values[name.Name] = unionType(options...)
	}
}

func (c *checker) resolveTypeRef(ref TypeRef) (Type, error) {
	if len(ref.Options) > 0 {
		options := make([]Type, 0, len(ref.Options))
		for _, optionRef := range ref.Options {
			option, err := c.resolveTypeRef(optionRef)
			if err != nil {
				return Type{}, err
			}
			options = append(options, option)
		}
		return unionType(options...), nil
	}
	if ref.Name == "Literal" {
		return Type{Kind: TypeLiteral, Name: ref.Literal}, nil
	}
	name := ref.Name
	if alias, ok := c.aliases[name]; ok {
		return alias, nil
	}
	switch name {
	case "", "Any":
		return AnyType(), nil
	case "int":
		return IntType(), nil
	case "float":
		return FloatType(), nil
	case "str":
		return StrType(), nil
	case "bool":
		return BoolType(), nil
	case "None", "void":
		return NoneType(), nil
	case "TyphonTask":
		return TaskType(), nil
	case "PyObject":
		return PyType(), nil
	case "list":
		if ref.Elem == nil {
			return Type{Kind: TypeList, Name: "list"}, nil
		}
		elem, err := c.resolveTypeRef(*ref.Elem)
		if err != nil {
			return Type{}, err
		}
		return Type{Kind: TypeList, Name: "list", Elem: typePtr(elem)}, nil
	default:
		if classInfo, ok := c.classes[name]; ok {
			return Type{Kind: TypeClass, Name: name, Class: classInfo}, nil
		}
		if typ, ok := c.globals[name]; ok {
			return typ, nil
		}
		return Type{}, fmt.Errorf("unknown type %q", name)
	}
}

func (c *checker) fail(line int, message string) {
	c.errors = append(c.errors, syntaxError(c.module.Path, line, message))
}

type scope struct {
	initialized map[string]bool
	module      bool
	values      map[string]Type
	parent      *scope
	globals     map[string]Type
}

func newScope(globals map[string]Type) *scope {
	return &scope{values: map[string]Type{}, initialized: map[string]bool{}, globals: globals}
}

func (s *scope) child() *scope {
	child := newScope(s.globals)
	child.parent = s
	for name, initialized := range s.initialized {
		child.initialized[name] = initialized
	}
	return child
}

func (s *scope) set(name string, typ Type) {
	s.values[name] = typ
	s.initialized[name] = true
}

func alwaysReturns(stmts []Stmt) bool {
	for _, stmt := range stmts {
		if _, ok := stmt.(*ReturnStmt); ok {
			return true
		}
		if branch, ok := stmt.(*IfStmt); ok && alwaysReturns(branch.Body) && alwaysReturns(branch.Else) {
			return true
		}
	}
	return false
}

func (c *checker) declareLocals(stmts []Stmt, scope *scope) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *AnnAssignStmt:
			if name, ok := s.Target.(*NameExpr); ok {
				typ, _ := c.resolveTypeRef(s.Type)
				scope.set(name.Name, typ)
				scope.initialized[name.Name] = false
			}
		case *IfStmt:
			c.declareLocals(s.Body, scope)
			c.declareLocals(s.Else, scope)
		case *WhileStmt:
			c.declareLocals(s.Body, scope)
		case *ForStmt:
			for _, target := range s.Targets {
				typ, _ := c.resolveTypeRef(target.Type)
				scope.set(target.Name, typ)
				scope.initialized[target.Name] = false
			}
			c.declareLocals(s.Body, scope)
		}
	}
}

func (c *checker) inferExpected(expr Expr, expected Type, scope *scope, classInfo *ClassInfo) Type {
	if list, ok := expr.(*ListExpr); ok && expected.Kind == TypeList && expected.Elem != nil {
		for _, item := range list.Elements {
			actual := c.inferExpected(item, *expected.Elem, scope, classInfo)
			if !assignable(actual, *expected.Elem) {
				c.fail(item.LineNo(), fmt.Sprintf("list element expected %s, got %s", expected.Elem, actual))
			}
		}
		return expected
	}
	return c.inferExpr(expr, scope, classInfo)
}

func binaryType(op string, left, right Type) (Type, error) {
	if left.Kind == TypeLiteral {
		left = StrType()
	}
	if right.Kind == TypeLiteral {
		right = StrType()
	}
	if left.Kind == TypeAny || left.Kind == TypePy || right.Kind == TypeAny || right.Kind == TypePy {
		return AnyType(), nil
	}
	if (op == "+" || op == "compare") && left.Kind == TypeStr && right.Kind == TypeStr {
		return StrType(), nil
	}
	if op == "*" && (left.Kind == TypeStr && right.Kind == TypeInt || left.Kind == TypeInt && right.Kind == TypeStr) {
		return StrType(), nil
	}
	if (left.Kind == TypeInt || left.Kind == TypeFloat) && (right.Kind == TypeInt || right.Kind == TypeFloat) {
		if op == "/" {
			return FloatType(), nil
		}
		if left.Kind == TypeFloat || right.Kind == TypeFloat {
			return FloatType(), nil
		}
		return IntType(), nil
	}
	return AnyType(), fmt.Errorf("operator %s does not support %s and %s", op, left, right)
}

func (s *scope) lookup(name string) (Type, bool) {
	for current := s; current != nil; current = current.parent {
		if typ, ok := current.values[name]; ok {
			return typ, true
		}
	}
	if s.globals != nil {
		typ, ok := s.globals[name]
		return typ, ok
	}
	return Type{}, false
}

func typePtr(typ Type) *Type {
	return &typ
}
