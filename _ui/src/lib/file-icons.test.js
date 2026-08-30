import assert from "node:assert/strict";
import test from "node:test";

import { FILE_ICON_NAMES, resolveFileIcon } from "./file-icons.js";
import { vscodeIcons } from "./file-icons.generated.js";

test("maps common languages and repository metadata", () => {
  assert.equal(resolveFileIcon("main.go"), "vscode-icons:file-type-go");
  assert.equal(resolveFileIcon("Component.TSX"), "vscode-icons:file-type-reactts");
  assert.equal(resolveFileIcon("src/lib/App.svelte"), "vscode-icons:file-type-svelte");
  assert.equal(resolveFileIcon("schema.proto"), "vscode-icons:file-type-protobuf");
  assert.equal(resolveFileIcon("go.mod"), "vscode-icons:file-type-go-package");
  assert.equal(resolveFileIcon(".env.production"), "vscode-icons:file-type-dotenv");
});

test("maps compound configuration, test, and declaration filenames", () => {
  assert.equal(resolveFileIcon(".eslintrc.js"), "vscode-icons:file-type-eslint");
  assert.equal(resolveFileIcon("tsconfig.browser.json"), "vscode-icons:file-type-tsconfig");
  assert.equal(resolveFileIcon("widget.test.js"), "vscode-icons:file-type-testjs");
  assert.equal(resolveFileIcon("widget.spec.tsx"), "vscode-icons:file-type-testts");
  assert.equal(resolveFileIcon("parser.test.go"), "vscode-icons:file-type-test");
  assert.equal(resolveFileIcon("contracts.d.ts"), "vscode-icons:file-type-typescriptdef-official");
});

test("maps known folders in both states and falls back for unknown entries", () => {
  assert.equal(resolveFileIcon("src", { isDir: true }), "vscode-icons:folder-type-src");
  assert.equal(
    resolveFileIcon("src", { isDir: true, expanded: true }),
    "vscode-icons:folder-type-src-opened",
  );
  assert.equal(resolveFileIcon("unknown.xyz"), "vscode-icons:default-file");
  assert.equal(resolveFileIcon("unknown", { isDir: true }), "vscode-icons:default-folder");
  assert.equal(
    resolveFileIcon("unknown", { isDir: true, expanded: true }),
    "vscode-icons:default-folder-opened",
  );
});

test("generated collection contains exactly the curated icon set", () => {
  assert.equal(vscodeIcons.prefix, "vscode-icons");
  assert.deepEqual(Object.keys(vscodeIcons.icons).sort(), [...FILE_ICON_NAMES]);
  for (const icon of FILE_ICON_NAMES) assert.ok(vscodeIcons.icons[icon]?.body, `${icon} has SVG data`);
});
