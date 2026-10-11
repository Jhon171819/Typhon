(identifier) @variable
(keyword) @keyword
["def" "class" "type"] @keyword
(constant) @constant
(number) @number
(string) @string
(escape_sequence) @string.escape
(comment) @comment
(operator) @operator
(delimiter) @punctuation.delimiter
["(" ")" "[" "]" "{" "}"] @punctuation.bracket

((identifier) @type.builtin
  (#match? @type.builtin "^(int|float|str|bool|void|list|dict|PyObject|TyphonTask)$"))
((identifier) @function.builtin
  (#match? @function.builtin "^(print|range|enumerate)$"))
((identifier) @variable.special
  (#eq? @variable.special "self"))
(function_header name: (identifier) @function)
(class_header name: (identifier) @type)
(type_alias_header name: (identifier) @type)
