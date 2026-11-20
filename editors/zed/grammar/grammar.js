// An editor grammar: recognize tokens and balanced groups, without enforcing
// indentation or type semantics. The native Typhon checker owns diagnostics.
module.exports = grammar({
  name: 'typhon',
  extras: _ => [/\s/],
  word: $ => $.identifier,
  rules: {
    source_file: $ => repeat($._item),
    _item: $ => choice(
      $.function_header, $.class_header, $.type_alias_header,
      $.parenthesized, $.list, $.braced,
      $.string, $.comment, $.number, $.constant, $.keyword,
      $.identifier, $.operator, $.delimiter,
    ),
    function_header: $ => prec(1, seq('def', field('name', $.identifier))),
    class_header: $ => prec(1, seq('class', field('name', $.identifier))),
    type_alias_header: $ => prec(1, seq('type', field('name', $.identifier))),
    parenthesized: $ => seq('(', repeat($._item), ')'),
    list: $ => seq('[', repeat($._item), ']'),
    braced: $ => seq('{', repeat($._item), '}'),
    identifier: _ => /[A-Za-z_][A-Za-z_0-9]*/,
    keyword: _ => choice(
      'def', 'class', 'type', 'if', 'elif', 'else', 'while', 'for',
      'in', 'break', 'continue', 'return', 'from', 'import', 'as',
      'and', 'or', 'not', 'is',
    ),
    constant: _ => choice('True', 'False', 'None'),
    number: _ => /[0-9]+(\.[0-9]+)?/,
    string: $ => seq(
      choice(
        seq(choice('"', 'f"', 'F"'), repeat(choice($.double_string_content, $.escape_sequence)), '"'),
        seq(choice("'", "f'", "F'"), repeat(choice($.single_string_content, $.escape_sequence)), "'"),
      ),
    ),
    double_string_content: _ => token.immediate(/[^"\\\r\n]+/),
    single_string_content: _ => token.immediate(/[^'\\\r\n]+/),
    escape_sequence: _ => token.immediate(/\\[^\r\n]/),
    comment: _ => token(seq('#', /[^\r\n]*/)),
    operator: _ => choice('->', '==', '!=', '<=', '>=', '//', '+', '-', '*', '/', '%', '=', '<', '>', '|'),
    delimiter: _ => choice(':', ',', '.'),
  },
});
