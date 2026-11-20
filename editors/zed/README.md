# Typhon for Zed

Recognizes `.ty` as **Typhon**, using a dedicated Tree-sitter grammar called
`typhon`. Provides syntax highlighting, paired brackets/quotes, `#` comments and
four-space indentation, including typed `for` targets and Typhon built-in types.
It registers no Python language server. Actual `.py` files retain Python mode.

## Install on Windows

Run the installer from this directory or the extracted prebuilt ZIP:

```powershell
./install.ps1
```

The ZIP includes the compiled `grammars/typhon.wasm`, so installation needs
PowerShell and Git but no compiler, Rust, or Node.js. Restart Zed and open a `.ty`
file; the language selector should show **Typhon**.

The installer copies only this extension to
`$env:LOCALAPPDATA/Zed/extensions/installed/typhon`. It creates a local grammar
source Git snapshot under `extensions/work/typhon-grammar`, and supplies its
file URL and revision in the installed manifest. This lets Zed rebuild the
grammar locally before these changes are published. It does not commit changes
in the Typhon project or publish anything.

The repository's `.zed/settings.json` maps `.ty` explicitly to **Typhon**,
disables language servers for it and turns off automatic formatting. In another
workspace, remove a Python file association for `.ty` and add:

```json
{
  "file_types": { "Typhon": ["ty"] },
  "languages": {
    "Typhon": { "enable_language_server": false, "format_on_save": "off" }
  }
}
```

## Build and validate

From the Typhon source checkout (with `examples/` and `.zed/`), install Node.js and a Windows x86_64
[wasi-sdk](https://github.com/WebAssembly/wasi-sdk/releases), then run:

```powershell
./build.ps1 -WasiSdkPath C:/path/to/wasi-sdk
```

This generates a Tree-sitter ABI 14 parser, compiles the WASM using wasi-sdk and
tests the compiled parser plus Zed's highlighting/bracket/scope queries against
Typhon-specific fixtures and all `.ty` examples. To repeat only the tests:

```powershell
cd grammar
npm ci
npm test
```

This is a lightweight editor grammar for tokens and balanced groups. It does not
validate indentation, mandatory type annotations or runtime behavior; the native
Typhon checker remains responsible for these. F-strings are highlighted as
strings, including their interpolated contents. There is no completion,
go-to-definition, semantic diagnostics or debugger integration yet.

## References

- [Zed language extension format](https://zed.dev/docs/extensions/languages)
- [Zed language file associations](https://zed.dev/docs/configuring-languages)
