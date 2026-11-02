const { test } = require('node:test');
const assert = require('node:assert/strict');
const Module = require('node:module');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL, fileURLToPath } = require('node:url');
const { fixture } = require('../test/project');

test('VS Code client routes a real server save, revert, add and delete into Problems', async t => {
  const f = fixture(); t.after(() => f.dispose());
  const subscriptions = [], commands = new Map(), problems = new Map(), errors = [];
  const disposable = () => ({ dispose() {} });
  const uri = text => ({ fsPath: fileURLToPath(text), toString: () => text });
  const vscode = {
    Uri: { file: file => uri(pathToFileURL(file).href), parse: uri },
    Range: class { constructor(a, b, c, d) { this.start = { line: a, character: b }; this.end = { line: c, character: d }; } },
    Diagnostic: class { constructor(range, message, severity) { Object.assign(this, { range, message, severity }); } },
    DiagnosticSeverity: { Error: 0, Warning: 1, Information: 2, Hint: 3 },
    languages: { createDiagnosticCollection: () => ({ set(u, values) { problems.set(u.toString(), values); }, dispose() {} }) },
    window: { createOutputChannel: () => ({ append() {}, appendLine() {}, dispose() {} }), showWarningMessage() {}, showErrorMessage: e => errors.push(e), showInformationMessage() {} },
    workspace: {
      workspaceFolders: [{ uri: uri(pathToFileURL(f.project).href), name: 'fixture' }],
      getConfiguration: () => ({ get: () => process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync') }),
      createFileSystemWatcher: () => ({ onDidChange: disposable, onDidCreate: disposable, onDidDelete: disposable, dispose() {} }),
      onDidChangeConfiguration: disposable, onDidChangeWorkspaceFolders: disposable
    },
    commands: { registerCommand(name, handler) { commands.set(name, handler); return disposable(); } }
  };
  const load = Module._load;
  Module._load = function(name, ...args) { return name === 'vscode' ? vscode : load.call(this, name, ...args); };
  let extension;
  try { extension = require('./extension'); } finally { Module._load = load; }
  extension.activate({ subscriptions });
  t.after(() => { for (const d of subscriptions.reverse()) d.dispose(); });
  const wait = async predicate => {
    const deadline = Date.now() + 5000;
    while (!predicate()) { assert.ok(Date.now() < deadline, `diagnostics timed out; ${errors.join(';')}`); await new Promise(r => setTimeout(r, 20)); }
  };
  const key = file => pathToFileURL(file).href;
  const has = (file, code) => (problems.get(key(file)) || []).some(d => d.code === code);
  await commands.get('stele.sync')();
  assert.deepEqual(errors, []);
  fs.writeFileSync(f.file, f.original.replace('locked', 'unlocked'));
  await wait(() => has(f.file, 'spec.changed'));
  assert.equal(problems.get(key(f.file))[0].source, 'Stele');
  fs.writeFileSync(f.file, f.original);
  await wait(() => problems.has(key(f.file)) && problems.get(key(f.file)).length === 0);
  const added = path.join(f.specs, 'new.md'); fs.writeFileSync(added, 'REQ-2: Keys MUST work.\n');
  await wait(() => has(added, 'spec.added'));
  fs.unlinkSync(added); await wait(() => problems.get(key(added))?.length === 0);
  fs.unlinkSync(f.file); await wait(() => has(path.join(f.registry, 'rules.yaml'), 'spec.removed'));
  assert.deepEqual(errors, []);
});
