const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const requireGrammar = require('node:module').createRequire(path.join(__dirname, 'grammar/package.json'));
const { Parser, Language, Query } = requireGrammar('web-tree-sitter');

async function main() {
  await Parser.init();
  const language = await Language.load(path.join(__dirname, 'grammars/typhon.wasm'));
  const parser = new Parser();
  parser.setLanguage(language);
  const queryPath = name => path.join(__dirname, 'languages/typhon', name + '.scm');
  const queries = ['highlights', 'brackets', 'overrides'].map(name =>
    new Query(language, fs.readFileSync(queryPath(name), 'utf8')));
  const fixtures = [
    ['for item: int in range(5):\n    print(item)', 'int', 'type.builtin'],
    ['def double(value: int) -> int:\n    return value * 2', 'double', 'function'],
    ['class User:\n    id: int', 'User', 'type'],
    ['type UserId = int', 'UserId', 'type'],
    ['value: PyObject = req.get(url)', 'PyObject', 'type.builtin'],
    ['task: TyphonTask = spawn(double, 21)', 'TyphonTask', 'type.builtin'],
    ['print(f"Request {item + 1}")', 'f"Request {item + 1}"', 'string'],
    ['value: str = "# not a comment" # comment', '# comment', 'comment'],
    ['value: str = "# not a comment" # comment', '"# not a comment"', 'string'],
  ];
  for (const [source, text, captureName] of fixtures) {
    const tree = parser.parse(source);
    assert.ok(!tree.rootNode.hasError, `${source}: ${tree.rootNode}`);
    const captures = queries[0].captures(tree.rootNode);
    assert.ok(captures.some(c => c.name === captureName && c.node.text === text),
      `${captureName}: ${text}: ${JSON.stringify(captures.map(c => [c.name, c.node.text]))}`);
    tree.delete();
  }
  const examples = path.join(__dirname, '../../examples');
  const files = fs.readdirSync(examples).filter(name => name.endsWith('.ty'));
  for (const file of files) {
    const tree = parser.parse(fs.readFileSync(path.join(examples, file), 'utf8'));
    assert.ok(!tree.rootNode.hasError, `${file}: ${tree.rootNode}`);
    for (const query of queries) query.captures(tree.rootNode);
    tree.delete();
  }
  const workspace = require('../../.zed/settings.json');
  assert.deepEqual(workspace.file_types.Typhon, ['ty']);
  assert.equal(workspace.languages.Typhon.enable_language_server, false);
  for (const query of queries) query.delete();
  parser.delete();
  console.log(`Typhon WASM parser and Zed queries passed (${files.length} examples).`);
}

main().catch(error => { console.error(error); process.exitCode = 1; });
