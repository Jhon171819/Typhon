package typhon

import "fmt"

type ImportResolver interface {
	ResolveImport(module string, fromDir string) (*ModuleInfo, error)
}

type checker struct {
	module   *Module
	resolver ImportResolver
	aliases  map[string]Type
	globals  map[string]Type
	classes  map[string]*ClassInfo
	errors   []error
}

func CheckModule(module *Module, resolver ImportResolver) (*ModuleInfo, error) {
	c := &checker{
		module:   module,
		resolver: resolver,
		aliases:  map[string]Type{},
		globals:  builtinTypes(),
		classes:  map[string]*ClassInfo{},
	}
	c.collect(module.Stmts)
	c.checkStatements(module.Stmts, newScope(nil), nil, NoneType())
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
		"print":       {Kind: TypeFunc, Name: "print", Params: []Type{AnyType()}, Return: typePtr(NoneType())},
		"spawn":       {Kind: TypeFunc, Name: "spawn", Params: []Type{AnyType()}, Return: typePtr(TaskType())},
		"join":        {Kind: TypeFunc, Name: "join", Params: []Type{TaskType()}, Return: typePtr(AnyType())},
		"enumerate":   {Kind: TypeFunc, Name: "enumerate", Params: []Type{AnyType()}, Return: typePtr(Type{Kind: TypeList, Name: "list", Elem: typePtr(AnyType())})},
		"range":       {Kind: TypeFunc, Name: "range", Params: []Type{AnyType()}, Return: typePtr(Type{Kind: TypeList, Name: "list", Elem: typePtr(IntType())})},
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
	case "print", "spawn", "join", "enumerate", "range", "TyphonTask", "PyObject", "int", "float", "str", "bool", "None", "void", "list", "__name__", "__file__", "__package__":
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
			classInfo := &ClassInfo{Name: s.Name, Fields: map[string]Type{}, Methods: map[string]Type{}}
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
	return Type{Kind: TypeFunc, Name: fn.Name, Params: params, Return: typePtr(ret)}
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
		var actual Type
		if s.Value == nil {
			actual = NoneType()
		} else {
			actual = c.inferExpr(s.Value, scope, classInfo)
		}
		if !assignable(actual, expectedReturn) {
			c.fail(s.Line, fmt.Sprintf("return expected %s, got %s", expectedReturn.String(), actual.String()))
		}
	case *IfStmt:
		c.inferExpr(s.Cond, scope, classInfo)
		c.checkStatements(s.Body, scope.child(), classInfo, expectedReturn)
	case *ForStmt:
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
		}
		c.checkStatements(s.Body, child, classInfo, expectedReturn)
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
	fnType := c.functionType(fn, classInfo)
	ret := NoneType()
	if fnType.Return != nil {
		ret = *fnType.Return
	}
	child := newScope(c.globals)
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
}

func (c *checker) checkClass(classDef *ClassDefStmt) {
	classInfo := c.classes[classDef.Name]
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
		actual := c.inferExpr(stmt.Value, scope, classInfo)
		if actual.Kind == TypeList {
			if listExpr, ok := stmt.Value.(*ListExpr); ok && len(listExpr.Elements) == 0 {
				actual = typ
			}
		}
		if !assignable(actual, typ) {
			c.fail(stmt.Line, fmt.Sprintf("assignment expected %s, got %s", typ.String(), actual.String()))
		}
	}
	switch target := stmt.Target.(type) {
	case *NameExpr:
		scope.set(target.Name, typ)
		if scope.globals != nil && scope.parent == nil {
			scope.globals[target.Name] = typ
		}
	case *AttrExpr:
		c.checkAttrAssign(stmt.Line, target, typ, scope, classInfo)
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
	case *AttrExpr:
		objType := c.inferExpr(target.Object, scope, classInfo)
		if objType.Kind == TypeClass && objType.Class != nil {
			expected, ok := objType.Class.Fields[target.Name]
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
	case *StringExpr, *FStringExpr:
		return StrType()
	case *BoolExpr:
		return BoolType()
	case *NoneExpr:
		return NoneType()
	case *ListExpr:
		if len(e.Elements) == 0 {
			return Type{Kind: TypeList, Name: "list", Elem: typePtr(AnyType())}
		}
		elem := c.inferExpr(e.Elements[0], scope, classInfo)
		for _, item := range e.Elements[1:] {
			itemType := c.inferExpr(item, scope, classInfo)
			if !assignable(itemType, elem) {
				elem = AnyType()
				break
			}
		}
		return Type{Kind: TypeList, Name: "list", Elem: typePtr(elem)}
	case *BinaryExpr:
		left := c.inferExpr(e.Left, scope, classInfo)
		right := c.inferExpr(e.Right, scope, classInfo)
		switch e.Op {
		case "==", "!=", "is", "is not":
			return BoolType()
		case "+", "-":
			if left.Kind == TypeStr || right.Kind == TypeStr {
				return StrType()
			}
			if left.Kind == TypeFloat || right.Kind == TypeFloat {
				return FloatType()
			}
			return IntType()
		case "*":
			if left.Kind == TypeStr || right.Kind == TypeStr {
				return StrType()
			}
			if left.Kind == TypeFloat || right.Kind == TypeFloat {
				return FloatType()
			}
			return IntType()
		default:
			return AnyType()
		}
	case *AttrExpr:
		return c.inferAttr(e, scope, classInfo)
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
	if objType.Kind == TypeStr {
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
	return AnyType()
}

func (c *checker) inferCall(expr *CallExpr, scope *scope, classInfo *ClassInfo) Type {
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
	c.checkCallArgs(expr.Line, calleeType.Params, expr.Args, scope, classInfo)
	if calleeType.Return == nil {
		return NoneType()
	}
	return *calleeType.Return
}

func (c *checker) checkCallArgs(line int, params []Type, args []Expr, scope *scope, classInfo *ClassInfo) {
	if len(params) > 0 && params[0].Kind == TypeAny {
		for _, arg := range args {
			c.inferExpr(arg, scope, classInfo)
		}
		return
	}
	if len(args) != len(params) {
		c.fail(line, fmt.Sprintf("call expected %d arguments, got %d", len(params), len(args)))
		return
	}
	for i, arg := range args {
		actual := c.inferExpr(arg, scope, classInfo)
		if !assignable(actual, params[i]) {
			c.fail(line, fmt.Sprintf("argument %d expected %s, got %s", i+1, params[i].String(), actual.String()))
		}
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
		return Type{Kind: TypeUnion, Options: options}, nil
	}
	if ref.Literal != "" {
		return StrType(), nil
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
	values  map[string]Type
	parent  *scope
	globals map[string]Type
}

func newScope(globals map[string]Type) *scope {
	return &scope{values: map[string]Type{}, globals: globals}
}

func (s *scope) child() *scope {
	return &scope{values: map[string]Type{}, parent: s, globals: s.globals}
}

func (s *scope) set(name string, typ Type) {
	s.values[name] = typ
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
