package typhon

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type sourceLine struct {
	line   int
	indent int
	text   string
}

type parser struct {
	path  string
	lines []sourceLine
	pos   int
}

func ParseFile(path string, source string) (*Module, error) {
	lines, err := logicalLines(source)
	if err != nil {
		return nil, err
	}
	p := &parser{path: path, lines: lines}
	stmts, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		line := p.lines[p.pos]
		return nil, syntaxError(path, line.line, "unexpected indentation")
	}
	return &Module{Path: path, Dir: filepath.Dir(path), Stmts: stmts}, nil
}

func logicalLines(source string) ([]sourceLine, error) {
	raw := strings.Split(source, "\n")
	var lines []sourceLine
	var current strings.Builder
	startLine := 0
	startIndent := 0
	depth := 0

	for i, rawLine := range raw {
		lineNo := i + 1
		line := strings.TrimRight(rawLine, "\r\t ")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		indent := countIndent(line)
		if current.Len() == 0 {
			startLine = lineNo
			startIndent = indent
		} else {
			current.WriteByte(' ')
		}
		noComment := stripComment(strings.TrimSpace(line))
		current.WriteString(noComment)
		depth += bracketDelta(noComment)
		if depth < 0 {
			return nil, syntaxError("", lineNo, "unmatched closing delimiter")
		}
		if depth == 0 {
			lines = append(lines, sourceLine{line: startLine, indent: startIndent, text: strings.TrimSpace(current.String())})
			current.Reset()
		}
	}
	if current.Len() != 0 {
		return nil, syntaxError("", startLine, "unterminated multiline statement")
	}
	return lines, nil
}

func countIndent(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' {
			count++
			continue
		}
		break
	}
	return count
}

func stripComment(line string) string {
	inString := rune(0)
	escaped := false
	for i, ch := range line {
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		if ch == '#' {
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

func bracketDelta(line string) int {
	delta := 0
	inString := rune(0)
	escaped := false
	for _, ch := range line {
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			delta++
		case ')', ']', '}':
			delta--
		}
	}
	return delta
}

func (p *parser) parseBlock(indent int) ([]Stmt, error) {
	var stmts []Stmt
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, syntaxError(p.path, line.line, "unexpected indentation")
		}
		stmt, err := p.parseStmt(indent)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt)
	}
	return stmts, nil
}

func (p *parser) parseStmt(indent int) (Stmt, error) {
	line := p.lines[p.pos]
	text := line.text
	p.pos++

	switch {
	case strings.HasPrefix(text, "type "):
		return p.parseTypeAlias(line)
	case strings.HasPrefix(text, "from "):
		return p.parseImportFrom(line)
	case strings.HasPrefix(text, "import "):
		return p.parseImport(line)
	case strings.HasPrefix(text, "class "):
		return p.parseClass(line, indent)
	case strings.HasPrefix(text, "def "):
		return p.parseFunc(line, indent)
	case strings.HasPrefix(text, "if ") && strings.HasSuffix(text, ":"):
		return p.parseIf(line, indent)
	case strings.HasPrefix(text, "for ") && strings.HasSuffix(text, ":"):
		return p.parseFor(line, indent)
	case strings.HasPrefix(text, "return"):
		return p.parseReturn(line)
	default:
		return p.parseSimpleStmt(line)
	}
}

func (p *parser) parseTypeAlias(line sourceLine) (Stmt, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line.text, "type "))
	name, value, ok := strings.Cut(rest, "=")
	if !ok {
		return nil, syntaxError(p.path, line.line, "type alias must use '='")
	}
	typ, err := parseTypeRef(value)
	if err != nil {
		return nil, syntaxError(p.path, line.line, err.Error())
	}
	return &TypeAliasStmt{Line: line.line, Name: strings.TrimSpace(name), Value: typ}, nil
}

func (p *parser) parseImportFrom(line sourceLine) (Stmt, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line.text, "from "))
	module, namesText, ok := strings.Cut(rest, " import ")
	if !ok {
		return nil, syntaxError(p.path, line.line, "from import statement must use 'from module import name'")
	}
	names := splitTopLevel(namesText, ',')
	if len(names) == 0 {
		return nil, syntaxError(p.path, line.line, "import list cannot be empty")
	}
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	return &ImportFromStmt{Line: line.line, Module: strings.TrimSpace(module), Names: names}, nil
}

func (p *parser) parseImport(line sourceLine) (Stmt, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line.text, "import "))
	name := rest
	alias := ""
	if left, right, ok := strings.Cut(rest, " as "); ok {
		name = strings.TrimSpace(left)
		alias = strings.TrimSpace(right)
	}
	return &ImportStmt{Line: line.line, Name: name, Alias: alias}, nil
}

func (p *parser) parseClass(line sourceLine, indent int) (Stmt, error) {
	if !strings.HasSuffix(line.text, ":") {
		return nil, syntaxError(p.path, line.line, "class definition must end with ':'")
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line.text, "class "), ":"))
	if name == "" {
		return nil, syntaxError(p.path, line.line, "class name cannot be empty")
	}
	if p.pos >= len(p.lines) || p.lines[p.pos].indent <= indent {
		return nil, syntaxError(p.path, line.line, "class body cannot be empty")
	}
	bodyIndent := p.lines[p.pos].indent
	body, err := p.parseBlock(bodyIndent)
	if err != nil {
		return nil, err
	}
	classDef := &ClassDefStmt{Line: line.line, Name: name, Body: body}
	for _, stmt := range body {
		switch s := stmt.(type) {
		case *AnnAssignStmt:
			nameExpr, ok := s.Target.(*NameExpr)
			if !ok || s.Value != nil {
				continue
			}
			classDef.Fields = append(classDef.Fields, FieldDef{Line: s.Line, Name: nameExpr.Name, Type: s.Type})
		case *FuncDefStmt:
			classDef.Methods = append(classDef.Methods, s)
		}
	}
	return classDef, nil
}

func (p *parser) parseFunc(line sourceLine, indent int) (Stmt, error) {
	fn, err := parseFuncHeader(p.path, line)
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.lines) || p.lines[p.pos].indent <= indent {
		return nil, syntaxError(p.path, line.line, "function body cannot be empty")
	}
	bodyIndent := p.lines[p.pos].indent
	body, err := p.parseBlock(bodyIndent)
	if err != nil {
		return nil, err
	}
	fn.Body = body
	return fn, nil
}

func parseFuncHeader(path string, line sourceLine) (*FuncDefStmt, error) {
	text := strings.TrimSpace(strings.TrimPrefix(line.text, "def "))
	if !strings.HasSuffix(text, ":") {
		return nil, syntaxError(path, line.line, "function definition must end with ':'")
	}
	text = strings.TrimSpace(strings.TrimSuffix(text, ":"))
	open := strings.Index(text, "(")
	if open < 1 {
		return nil, syntaxError(path, line.line, "function definition needs a parameter list")
	}
	close := findMatching(text, open, '(', ')')
	if close < 0 {
		return nil, syntaxError(path, line.line, "function parameter list is not closed")
	}
	name := strings.TrimSpace(text[:open])
	paramsText := text[open+1 : close]
	after := strings.TrimSpace(text[close+1:])
	if !strings.HasPrefix(after, "->") {
		return nil, syntaxError(path, line.line, fmt.Sprintf("function '%s' must declare a return type", name))
	}
	returnType, err := parseTypeRef(strings.TrimSpace(strings.TrimPrefix(after, "->")))
	if err != nil {
		return nil, syntaxError(path, line.line, err.Error())
	}
	var params []Param
	if strings.TrimSpace(paramsText) != "" {
		for _, part := range splitTopLevel(paramsText, ',') {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if part == "self" || part == "cls" {
				params = append(params, Param{Name: part, Type: TypeRef{}, Line: line.line})
				continue
			}
			paramName, typeText, ok := strings.Cut(part, ":")
			if !ok {
				return nil, syntaxError(path, line.line, fmt.Sprintf("parameter '%s' must have an explicit type", strings.TrimSpace(part)))
			}
			typ, err := parseTypeRef(typeText)
			if err != nil {
				return nil, syntaxError(path, line.line, err.Error())
			}
			params = append(params, Param{Name: strings.TrimSpace(paramName), Type: typ, Line: line.line})
		}
	}
	return &FuncDefStmt{Line: line.line, Name: name, Params: params, ReturnType: returnType}, nil
}

func (p *parser) parseIf(line sourceLine, indent int) (Stmt, error) {
	condText := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line.text, "if "), ":"))
	cond, err := parseExpr(condText, line.line)
	if err != nil {
		return nil, syntaxError(p.path, line.line, err.Error())
	}
	if p.pos >= len(p.lines) || p.lines[p.pos].indent <= indent {
		return nil, syntaxError(p.path, line.line, "if body cannot be empty")
	}
	bodyIndent := p.lines[p.pos].indent
	body, err := p.parseBlock(bodyIndent)
	if err != nil {
		return nil, err
	}
	return &IfStmt{Line: line.line, Cond: cond, Body: body}, nil
}

func (p *parser) parseFor(line sourceLine, indent int) (Stmt, error) {
	text := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line.text, "for "), ":"))
	left, iterText, ok := strings.Cut(text, " in ")
	if !ok {
		return nil, syntaxError(p.path, line.line, "for-loop must use 'for name: Type in value:'")
	}
	var targets []ForTarget
	for _, part := range splitTopLevel(left, ',') {
		nameText, typeText, ok := strings.Cut(part, ":")
		if !ok {
			return nil, syntaxError(p.path, line.line, "for-loop variables must have an explicit type")
		}
		itemType, err := parseTypeRef(typeText)
		if err != nil {
			return nil, syntaxError(p.path, line.line, err.Error())
		}
		targets = append(targets, ForTarget{Name: strings.TrimSpace(nameText), Type: itemType, Line: line.line})
	}
	if len(targets) == 0 {
		return nil, syntaxError(p.path, line.line, "for-loop variables must have an explicit type")
	}
	iterable, err := parseExpr(iterText, line.line)
	if err != nil {
		return nil, syntaxError(p.path, line.line, err.Error())
	}
	if p.pos >= len(p.lines) || p.lines[p.pos].indent <= indent {
		return nil, syntaxError(p.path, line.line, "for body cannot be empty")
	}
	bodyIndent := p.lines[p.pos].indent
	body, err := p.parseBlock(bodyIndent)
	if err != nil {
		return nil, err
	}
	return &ForStmt{
		Line:     line.line,
		Name:     targets[0].Name,
		ItemType: targets[0].Type,
		Targets:  targets,
		Iterable: iterable,
		Body:     body,
	}, nil
}

func (p *parser) parseReturn(line sourceLine) (Stmt, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line.text, "return"))
	if rest == "" {
		return &ReturnStmt{Line: line.line}, nil
	}
	expr, err := parseExpr(rest, line.line)
	if err != nil {
		return nil, syntaxError(p.path, line.line, err.Error())
	}
	return &ReturnStmt{Line: line.line, Value: expr}, nil
}

func (p *parser) parseSimpleStmt(line sourceLine) (Stmt, error) {
	if idx := topLevelAssign(line.text); idx >= 0 {
		left := strings.TrimSpace(line.text[:idx])
		right := strings.TrimSpace(line.text[idx+1:])
		if colon := topLevelColon(left); colon >= 0 {
			target, err := parseExpr(strings.TrimSpace(left[:colon]), line.line)
			if err != nil {
				return nil, syntaxError(p.path, line.line, err.Error())
			}
			typ, err := parseTypeRef(left[colon+1:])
			if err != nil {
				return nil, syntaxError(p.path, line.line, err.Error())
			}
			value, err := parseExpr(right, line.line)
			if err != nil {
				return nil, syntaxError(p.path, line.line, err.Error())
			}
			return &AnnAssignStmt{Line: line.line, Target: target, Type: typ, Value: value}, nil
		}
		target, err := parseExpr(left, line.line)
		if err != nil {
			return nil, syntaxError(p.path, line.line, err.Error())
		}
		value, err := parseExpr(right, line.line)
		if err != nil {
			return nil, syntaxError(p.path, line.line, err.Error())
		}
		return &AssignStmt{Line: line.line, Target: target, Value: value}, nil
	}
	if colon := topLevelColon(line.text); colon >= 0 {
		targetText := strings.TrimSpace(line.text[:colon])
		typeText := strings.TrimSpace(line.text[colon+1:])
		target, err := parseExpr(targetText, line.line)
		if err != nil {
			return nil, syntaxError(p.path, line.line, err.Error())
		}
		typ, err := parseTypeRef(typeText)
		if err != nil {
			return nil, syntaxError(p.path, line.line, err.Error())
		}
		return &AnnAssignStmt{Line: line.line, Target: target, Type: typ}, nil
	}
	expr, err := parseExpr(line.text, line.line)
	if err != nil {
		return nil, syntaxError(p.path, line.line, err.Error())
	}
	return &ExprStmt{Line: line.line, Expr: expr}, nil
}

func parseTypeRef(text string) (TypeRef, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return TypeRef{}, fmt.Errorf("type annotation cannot be empty")
	}
	text = strings.TrimSuffix(text, ":")
	parts := splitTypeUnion(text)
	if len(parts) > 1 {
		options := make([]TypeRef, 0, len(parts))
		for _, part := range parts {
			typ, err := parseTypeRef(part)
			if err != nil {
				return TypeRef{}, err
			}
			options = append(options, typ)
		}
		return TypeRef{Options: options}, nil
	}
	if (strings.HasPrefix(text, "\"") && strings.HasSuffix(text, "\"")) || (strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'")) {
		value, err := unquotePythonString(text)
		if err != nil {
			return TypeRef{}, err
		}
		return TypeRef{Name: "Literal", Literal: value}, nil
	}
	if open := strings.Index(text, "["); open > 0 && strings.HasSuffix(text, "]") {
		elem, err := parseTypeRef(text[open+1 : len(text)-1])
		if err != nil {
			return TypeRef{}, err
		}
		return TypeRef{Name: strings.TrimSpace(text[:open]), Elem: &elem}, nil
	}
	if text == "void" {
		text = "None"
	}
	return TypeRef{Name: text}, nil
}

func splitTypeUnion(text string) []string {
	pipeParts := splitTopLevel(text, '|')
	if len(pipeParts) > 1 {
		return pipeParts
	}
	var parts []string
	for _, part := range strings.Split(text, " or ") {
		if strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) > 1 {
		return parts
	}
	return []string{text}
}

func topLevelAssign(text string) int {
	depth := 0
	inString := rune(0)
	escaped := false
	for i, ch := range text {
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '=':
			if depth == 0 {
				prev := byte(0)
				next := byte(0)
				if i > 0 {
					prev = text[i-1]
				}
				if i+1 < len(text) {
					next = text[i+1]
				}
				if prev != '=' && prev != '!' && next != '=' {
					return i
				}
			}
		}
	}
	return -1
}

func topLevelColon(text string) int {
	depth := 0
	inString := rune(0)
	escaped := false
	for i, ch := range text {
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitTopLevel(text string, sep rune) []string {
	var parts []string
	depth := 0
	inString := rune(0)
	escaped := false
	start := 0
	for i, ch := range text {
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if ch == sep && depth == 0 {
				parts = append(parts, strings.TrimSpace(text[start:i]))
				start = i + len(string(ch))
			}
		}
	}
	parts = append(parts, strings.TrimSpace(text[start:]))
	return parts
}

func findMatching(text string, open int, left rune, right rune) int {
	depth := 0
	inString := rune(0)
	escaped := false
	for i, ch := range text {
		if i < open {
			continue
		}
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		if ch == left {
			depth++
		}
		if ch == right {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func syntaxError(path string, line int, message string) error {
	if path == "" {
		return fmt.Errorf("line %d: %s", line, message)
	}
	return fmt.Errorf("%s:%d: %s", path, line, message)
}

type exprToken struct {
	kind string
	text string
	line int
}

func parseExpr(text string, line int) (Expr, error) {
	tokens, err := lexExpr(text, line)
	if err != nil {
		return nil, err
	}
	p := &exprParser{tokens: tokens, line: line}
	expr, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != "eof" {
		return nil, fmt.Errorf("unexpected token %q", p.peek().text)
	}
	return expr, nil
}

func lexExpr(text string, line int) ([]exprToken, error) {
	var tokens []exprToken
	for i := 0; i < len(text); {
		ch := rune(text[i])
		if unicode.IsSpace(ch) {
			i++
			continue
		}
		if unicode.IsLetter(ch) || ch == '_' || ((ch == 'f' || ch == 'F') && i+1 < len(text) && (text[i+1] == '"' || text[i+1] == '\'')) {
			if (ch == 'f' || ch == 'F') && i+1 < len(text) && (text[i+1] == '"' || text[i+1] == '\'') {
				raw, next, err := readQuoted(text, i+1)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, exprToken{kind: "fstring", text: raw, line: line})
				i = next
				continue
			}
			start := i
			for i < len(text) {
				r := rune(text[i])
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
					break
				}
				i++
			}
			tokens = append(tokens, exprToken{kind: "ident", text: text[start:i], line: line})
			continue
		}
		if unicode.IsDigit(ch) {
			start := i
			for i < len(text) && unicode.IsDigit(rune(text[i])) {
				i++
			}
			kind := "int"
			if i+1 < len(text) && text[i] == '.' && unicode.IsDigit(rune(text[i+1])) {
				kind = "float"
				i++
				for i < len(text) && unicode.IsDigit(rune(text[i])) {
					i++
				}
			}
			tokens = append(tokens, exprToken{kind: kind, text: text[start:i], line: line})
			continue
		}
		if ch == '"' || ch == '\'' {
			raw, next, err := readQuoted(text, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, exprToken{kind: "string", text: raw, line: line})
			i = next
			continue
		}
		if i+1 < len(text) {
			two := text[i : i+2]
			if two == "==" || two == "!=" {
				tokens = append(tokens, exprToken{kind: two, text: two, line: line})
				i += 2
				continue
			}
		}
		switch ch {
		case '(', ')', '[', ']', ',', '.', '+', '-', '*', '=', ':':
			tokens = append(tokens, exprToken{kind: string(ch), text: string(ch), line: line})
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q", ch)
		}
	}
	tokens = append(tokens, exprToken{kind: "eof", line: line})
	return tokens, nil
}

func readQuoted(text string, start int) (string, int, error) {
	quote := text[start]
	escaped := false
	for i := start + 1; i < len(text); i++ {
		if escaped {
			escaped = false
			continue
		}
		if text[i] == '\\' {
			escaped = true
			continue
		}
		if text[i] == quote {
			return text[start : i+1], i + 1, nil
		}
	}
	return "", 0, fmt.Errorf("unterminated string")
}

type exprParser struct {
	tokens []exprToken
	pos    int
	line   int
}

func (p *exprParser) peek() exprToken {
	if p.pos >= len(p.tokens) {
		return exprToken{kind: "eof", line: p.line}
	}
	return p.tokens[p.pos]
}

func (p *exprParser) advance() exprToken {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *exprParser) match(kind string) bool {
	if p.peek().kind == kind {
		p.advance()
		return true
	}
	return false
}

func (p *exprParser) parseExpression(minPrec int) (Expr, error) {
	left, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	for {
		op, prec := p.currentBinary()
		if prec < minPrec {
			break
		}
		line := p.peek().line
		if op == "is not" {
			p.advance()
			p.advance()
		} else {
			p.advance()
		}
		right, err := p.parseExpression(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Line: line, Left: left, Op: op, Right: right}
	}
	return left, nil
}

func (p *exprParser) currentBinary() (string, int) {
	tok := p.peek()
	switch tok.kind {
	case "ident":
		if tok.text == "is" {
			if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == "ident" && p.tokens[p.pos+1].text == "not" {
				return "is not", 3
			}
			return "is", 3
		}
	case "==", "!=":
		return tok.kind, 3
	case "+":
		return "+", 5
	case "-":
		return "-", 5
	case "*":
		return "*", 6
	}
	return "", -1
}

func (p *exprParser) parsePostfix() (Expr, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.match("."):
			name := p.advance()
			if name.kind != "ident" {
				return nil, fmt.Errorf("attribute name expected")
			}
			expr = &AttrExpr{Line: name.line, Object: expr, Name: name.text}
		case p.match("("):
			args, keywords, err := p.parseCallArgs(")")
			if err != nil {
				return nil, err
			}
			expr = &CallExpr{Line: expr.LineNo(), Callee: expr, Args: args, Keywords: keywords}
		default:
			return expr, nil
		}
	}
}

func (p *exprParser) parseCallArgs(end string) ([]Expr, []KeywordArg, error) {
	var args []Expr
	var keywords []KeywordArg
	if p.match(end) {
		return args, keywords, nil
	}
	for {
		if p.peek().kind == "ident" && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == "=" {
			name := p.advance().text
			p.advance()
			value, err := p.parseExpression(0)
			if err != nil {
				return nil, nil, err
			}
			keywords = append(keywords, KeywordArg{Name: name, Value: value})
			if p.match(end) {
				break
			}
			if !p.match(",") {
				return nil, nil, fmt.Errorf("expected ',' or '%s'", end)
			}
			if p.match(end) {
				break
			}
			continue
		}
		expr, err := p.parseExpression(0)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, expr)
		if p.match(end) {
			break
		}
		if !p.match(",") {
			return nil, nil, fmt.Errorf("expected ',' or '%s'", end)
		}
		if p.match(end) {
			break
		}
	}
	return args, keywords, nil
}

func (p *exprParser) parseArgs(end string) ([]Expr, error) {
	var args []Expr
	if p.match(end) {
		return args, nil
	}
	for {
		expr, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		args = append(args, expr)
		if p.match(end) {
			break
		}
		if !p.match(",") {
			return nil, fmt.Errorf("expected ',' or '%s'", end)
		}
		if p.match(end) {
			break
		}
	}
	return args, nil
}

func (p *exprParser) parsePrimary() (Expr, error) {
	tok := p.advance()
	switch tok.kind {
	case "ident":
		switch tok.text {
		case "True":
			return &BoolExpr{Line: tok.line, Value: true}, nil
		case "False":
			return &BoolExpr{Line: tok.line, Value: false}, nil
		case "None":
			return &NoneExpr{Line: tok.line}, nil
		default:
			return &NameExpr{Line: tok.line, Name: tok.text}, nil
		}
	case "int":
		value, err := strconv.ParseInt(tok.text, 10, 64)
		if err != nil {
			return nil, err
		}
		return &IntExpr{Line: tok.line, Value: value}, nil
	case "float":
		value, err := strconv.ParseFloat(tok.text, 64)
		if err != nil {
			return nil, err
		}
		return &FloatExpr{Line: tok.line, Value: value}, nil
	case "string":
		value, err := unquotePythonString(tok.text)
		if err != nil {
			return nil, err
		}
		return &StringExpr{Line: tok.line, Value: value}, nil
	case "fstring":
		return parseFString(tok.text, tok.line)
	case "(":
		expr, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		if !p.match(")") {
			return nil, fmt.Errorf("expected ')'")
		}
		return expr, nil
	case "[":
		elements, err := p.parseArgs("]")
		if err != nil {
			return nil, err
		}
		return &ListExpr{Line: tok.line, Elements: elements}, nil
	default:
		return nil, fmt.Errorf("unexpected token %q", tok.text)
	}
}

func parseFString(raw string, line int) (Expr, error) {
	value, err := unquotePythonString(raw)
	if err != nil {
		return nil, err
	}
	var parts []FStringPart
	for {
		open := strings.Index(value, "{")
		if open < 0 {
			if value != "" {
				parts = append(parts, FStringPart{Text: value})
			}
			break
		}
		if open > 0 {
			parts = append(parts, FStringPart{Text: value[:open]})
		}
		close := strings.Index(value[open+1:], "}")
		if close < 0 {
			return nil, fmt.Errorf("unterminated f-string expression")
		}
		exprText := strings.TrimSpace(value[open+1 : open+1+close])
		format := ""
		if exprPart, formatPart, ok := strings.Cut(exprText, ":"); ok {
			exprText = strings.TrimSpace(exprPart)
			format = strings.TrimSpace(formatPart)
		}
		expr, err := parseExpr(exprText, line)
		if err != nil {
			return nil, err
		}
		parts = append(parts, FStringPart{Expr: expr, Format: format})
		value = value[open+1+close+1:]
	}
	return &FStringExpr{Line: line, Parts: parts}, nil
}

func unquotePythonString(raw string) (string, error) {
	if len(raw) < 2 {
		return "", fmt.Errorf("invalid string literal")
	}
	quote := raw[0]
	if (quote != '\'' && quote != '"') || raw[len(raw)-1] != quote {
		return "", fmt.Errorf("invalid string literal")
	}

	body := raw[1 : len(raw)-1]
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if ch != '\\' {
			out.WriteByte(ch)
			continue
		}
		if i+1 >= len(body) {
			return "", fmt.Errorf("unterminated escape sequence")
		}
		i++
		switch body[i] {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case '\\':
			out.WriteByte('\\')
		case '\'':
			out.WriteByte('\'')
		case '"':
			out.WriteByte('"')
		default:
			out.WriteByte(body[i])
		}
	}
	return out.String(), nil
}
