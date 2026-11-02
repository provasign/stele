const vscode = require('vscode');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

// A small stdio LSP client, using only VS Code and Node built-ins.
function activate(context) {
  const output = vscode.window.createOutputChannel('Stele');
  const sessions = new Map();
  context.subscriptions.push(output);
  function start(folder) {
    const key = folder.uri.toString();
    const old = sessions.get(key);
    if (old) old.dispose();
    if (!fs.existsSync(path.join(folder.uri.fsPath, 'stele.yaml'))) return;
    const diagnostics = vscode.languages.createDiagnosticCollection(`stele:${key}`);
    const binary = vscode.workspace.getConfiguration('stele', folder.uri).get('serverPath', 'stele');
    const child = spawn(binary, ['lsp', '--project', folder.uri.fsPath], { cwd: folder.uri.fsPath, shell: false });
    let sequence = 0, buffer = Buffer.alloc(0), stopping = false;
    const pending = new Map();
    function send(method, params, id) {
      const body = JSON.stringify({ jsonrpc: '2.0', method, params, ...(id === undefined ? {} : { id }) });
      child.stdin.write(`Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);
    }
    function request(method, params) {
      return new Promise((resolve, reject) => {
        const id = ++sequence;
        const timer = setTimeout(() => { pending.delete(id); reject(new Error(`Stele request timed out: ${method}`)); }, 10000);
        pending.set(id, { resolve, reject, timer });
        send(method, params, id);
      });
    }
    function receive(message) {
      if (message.id !== undefined) {
        const entry = pending.get(message.id);
        if (entry) { pending.delete(message.id); clearTimeout(entry.timer); message.error ? entry.reject(new Error(message.error.message)) : entry.resolve(message.result); }
      } else if (message.method === 'textDocument/publishDiagnostics') {
        const p = message.params;
        diagnostics.set(vscode.Uri.parse(p.uri), p.diagnostics.map(d => {
          const range = new vscode.Range(d.range.start.line, d.range.start.character, d.range.end.line, d.range.end.character);
          const diagnostic = new vscode.Diagnostic(range, d.message, ({ 1: vscode.DiagnosticSeverity.Error, 2: vscode.DiagnosticSeverity.Warning, 3: vscode.DiagnosticSeverity.Information })[d.severity] ?? vscode.DiagnosticSeverity.Hint);
          diagnostic.source = 'Stele'; diagnostic.code = d.code;
          return diagnostic;
        }));
      } else if (message.method === 'window/showMessage') {
        output.appendLine(message.params.message);
        vscode.window.showWarningMessage(message.params.message);
      }
    }
    child.stdout.on('data', chunk => {
      buffer = Buffer.concat([buffer, chunk]);
      try {
        for (;;) {
          const end = buffer.indexOf('\r\n\r\n');
          if (end < 0) return;
          const match = /Content-Length:\s*(\d+)/i.exec(buffer.subarray(0, end).toString());
          if (!match || Number(match[1]) > 16 * 1024 * 1024) throw new Error('Invalid Stele LSP frame');
          const length = Number(match[1]);
          if (buffer.length < end + 4 + length) return;
          const message = JSON.parse(buffer.subarray(end + 4, end + 4 + length).toString());
          buffer = buffer.subarray(end + 4 + length); receive(message);
        }
      } catch (error) { output.appendLine(error.message); child.kill(); }
    });
    child.stderr.on('data', chunk => output.append(chunk.toString()));
    child.on('error', error => { output.appendLine(error.message); vscode.window.showErrorMessage(`Stele: ${error.message}`); });
    child.on('close', () => {
      diagnostics.dispose();
      for (const entry of pending.values()) { clearTimeout(entry.timer); entry.reject(new Error('Stele language server closed')); }
      pending.clear(); if (sessions.get(key)?.child === child) sessions.delete(key);
      if (!stopping) { output.appendLine(`Stele server stopped for ${folder.name}.`); vscode.window.showWarningMessage('Stele sync stopped; status is unknown. Use Stele: Restart Spec Sync to reconnect.'); }
    });
    child.stdin.on('error', error => output.appendLine(error.message));
    const session = { child, request, dispose() { stopping = true; diagnostics.dispose(); child.kill(); } };
    sessions.set(key, session);
    request('initialize', { processId: process.pid, rootUri: key, capabilities: {} })
      .then(() => send('initialized', {})).catch(error => output.appendLine(error.message));
  }
  const restart = () => { for (const folder of vscode.workspace.workspaceFolders || []) start(folder); };
  context.subscriptions.push(vscode.commands.registerCommand('stele.restart', restart));
  context.subscriptions.push(vscode.commands.registerCommand('stele.sync', async () => {
    if (sessions.size === 0) restart();
    for (const session of sessions.values()) {
      try { const r = await session.request('workspace/executeCommand', { command: 'stele.sync' }); vscode.window.showInformationMessage(r.in_sync ? 'Stele: no saved-spec drift detected.' : `Stele: sync needs attention (${r.pending.length} statements pending, ${r.stale_rules.length} stale rules, ${(r.orphan_rules || []).length} orphaned rules). See Problems.`); }
      catch (error) { vscode.window.showErrorMessage(error.message); }
    }
  }));
  const watcher = vscode.workspace.createFileSystemWatcher('**/stele.yaml');
  context.subscriptions.push(watcher, watcher.onDidChange(restart), watcher.onDidCreate(restart), watcher.onDidDelete(restart));
  context.subscriptions.push(vscode.workspace.onDidChangeConfiguration(e => { if (e.affectsConfiguration('stele.serverPath')) restart(); }));
  context.subscriptions.push(vscode.workspace.onDidChangeWorkspaceFolders(e => {
    for (const folder of e.removed) { const session = sessions.get(folder.uri.toString()); if (session) session.dispose(); }
    for (const folder of e.added) start(folder);
  }));
  context.subscriptions.push({ dispose() { for (const session of sessions.values()) session.dispose(); sessions.clear(); } });
  restart();
}
module.exports = { activate };
