const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const tm = require('vscode-textmate');
const onig = require('vscode-oniguruma');

async function main() {
  const manifest = require('./package.json');
  const language = manifest.contributes.languages[0];
  assert.equal(language.id, 'typhon');
  assert.deepEqual(language.extensions, ['.ty']);
  const configuration = require('./language-configuration.json');
  assert.equal(configuration.comments.lineComment, '#');
  const indent = new RegExp(configuration.indentationRules.increaseIndentPattern);
  assert.ok(indent.test('    for item: int in range(5): # typed loop'));
  assert.ok(!indent.test('    print("https:")'));
  const workspace = require('../../.vscode/settings.json');
  assert.equal(workspace['files.associations']['*.ty'], 'typhon');

  const wasm = fs.readFileSync(require.resolve('vscode-oniguruma/release/onig.wasm'));
  await onig.loadWASM(wasm.buffer.slice(wasm.byteOffset, wasm.byteOffset + wasm.byteLength));
  const grammarPath = path.join(__dirname, manifest.contributes.grammars[0].path);
  const registry = new tm.Registry({
    onigLib: Promise.resolve({
      createOnigScanner: patterns => new onig.OnigScanner(patterns),
      createOnigString: text => new onig.OnigString(text),
    }),
    loadGrammar: async scope => {
      assert.equal(scope, 'source.typhon', 'must not depend on a Python grammar');
      return tm.parseRawGrammar(fs.readFileSync(grammarPath, 'utf8'), grammarPath);
    },
  });
  const grammar = await registry.loadGrammar('source.typhon');
  function check(line, needle, scope) {
    const tokens = grammar.tokenizeLine(line).tokens;
    const start = line.indexOf(needle);
    assert.ok(start >= 0);
    const token = tokens.find(t => t.startIndex <= start && t.endIndex > start);
    assert.ok(token.scopes.includes(scope), `${line}: ${needle}: ${token.scopes}`);
    assert.ok(tokens.every(t => t.scopes[0] === 'source.typhon'));
    assert.ok(tokens.every(t => t.scopes.every(s => !s.endsWith('.python'))));
  }
  check('type UserId = int', 'UserId', 'entity.name.type.typhon');
  check('def double(value: int) -> int:', 'double', 'entity.name.function.typhon');
  check('for item: int in range(5):', 'for', 'keyword.control.typhon');
  check('for item: int in range(5):', 'int', 'support.type.typhon');
  check('task: TyphonTask = spawn(double, 21)', 'TyphonTask', 'support.type.typhon');
  check('value: PyObject = req.get(url)', 'PyObject', 'support.type.typhon');
  check('text: str = "# if 123" # actual comment', '# if', 'string.quoted.double.typhon');
  check('text: str = "# if 123" # actual comment', '# actual', 'comment.line.number-sign.typhon');
  check('print(f"Request {item + 1}")', '+', 'keyword.operator.typhon');
  check('print(f"Request {item + 1}")', '1', 'constant.numeric.typhon');
  check("print(F'Value {item}')", 'item', 'meta.embedded.expression.typhon');
  check('print(f"{{literal}}")', '{{', 'constant.character.escape.typhon');
  check('value: User | None = None', '|', 'keyword.operator.typhon');

  const examples = path.join(__dirname, '../../examples');
  const files = fs.readdirSync(examples).filter(name => name.endsWith('.ty'));
  for (const file of files) {
    let state = tm.INITIAL;
    for (const line of fs.readFileSync(path.join(examples, file), 'utf8').split(/\r?\n/)) {
      const result = grammar.tokenizeLine(line, state);
      assert.ok(result.tokens.every(t => t.scopes[0] === 'source.typhon'), file);
      state = result.ruleStack;
    }
  }
  registry.dispose();
  console.log(`Typhon registration and tokenization passed (${files.length} examples).`);
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
