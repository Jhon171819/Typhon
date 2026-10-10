# Typhon Language

Registers `.ty` as **Typhon** (`typhon`, TextMate scope `source.typhon`), with
syntax highlighting, `#` comments, paired quotes/brackets, indentation and
folding. There is no dependency on the Python grammar or Python language server.
This is editor recognition, not a semantic language server: completion,
diagnostics and debugging are not provided.

## VS Code and compatible editors

From this directory, package and install locally (Node.js required):

```powershell
npx --yes @vscode/vsce package --no-dependencies
code --install-extension typhon-language-0.1.0.vsix
```

Reload the editor and open a `.ty` file. The status bar should show **Typhon**.
Compatible editors such as Cursor can use **Extensions: Install from VSIX**.
The extension is distributed locally; it is not published on the Marketplace.

The repository's `.vscode/settings.json` maps `*.ty` explicitly to `typhon`,
overriding a user-level Python association. Install the extension to make that
language ID available. In other workspaces, remove any `*.ty: python` mapping
or set `"files.associations": { "*.ty": "typhon" }`.

Only `.ty` is registered. The actual `.py` launcher/bridge files keep Python mode.

## PyCharm / IntelliJ IDEA

1. Enable the **TextMate Bundles** plugin in Settings > Plugins.
2. In Settings > Editor > File Types, remove `*.ty` from **Python** if present.
   Also clear a per-file Python override if one was set.
3. In Settings > Editor > TextMate Bundles, click **+** and select this
   `editors/typhon` directory (the one containing `package.json`).
4. Reopen a `.ty` file and check that the Typhon highlighting is active.

JetBrains can import a VS Code grammar directory directly, so the same grammar
is shared by both IDE families.

## Validation

```powershell
npm ci
npm test
```

The check uses VS Code's TextMate tokenizer and Oniguruma engine to verify
Typhon-specific syntax and tokenize all repository examples. These two packages
are development dependencies only; the installed extension contains no runtime
JavaScript or dependencies.

## Sources

- [VS Code language registration](https://code.visualstudio.com/docs/languages/identifiers)
- [VS Code language configuration](https://code.visualstudio.com/api/language-extensions/language-configuration-guide)
- [JetBrains TextMate imports](https://www.jetbrains.com/help/idea/textmate.html)
- [PyCharm file associations](https://www.jetbrains.com/help/pycharm/creating-and-registering-file-types.html)
