const PREFIX = "vscode-icons";
const DEFAULT_FILE_ICON = "default-file";
const DEFAULT_FOLDER_ICON = "default-folder";

const COMPOUND_FILE_ICONS = {
  eslint: "file-type-eslint",
  tsconfig: "file-type-tsconfig",
  test: "file-type-test",
  testJavaScript: "file-type-testjs",
  testTypeScript: "file-type-testts",
  typeScriptDefinition: "file-type-typescriptdef-official",
};

const FILE_NAMES = {
  ".babelrc": "file-type-babel2",
  ".dockerignore": "file-type-docker2",
  ".editorconfig": "file-type-editorconfig",
  ".env": "file-type-dotenv",
  ".eslintignore": "file-type-eslint",
  ".eslintrc": "file-type-eslint",
  ".gitattributes": "file-type-git",
  ".gitignore": "file-type-git",
  ".gitmodules": "file-type-git",
  ".npmignore": "file-type-npm",
  ".npmrc": "file-type-npm",
  ".nvmrc": "file-type-node",
  ".prettierignore": "file-type-prettier",
  ".prettierrc": "file-type-prettier",
  "bun.lock": "file-type-bun",
  "bun.lockb": "file-type-bun",
  "cargo.lock": "file-type-cargo",
  "cargo.toml": "file-type-cargo",
  "compose.yaml": "file-type-docker2",
  "compose.yml": "file-type-docker2",
  "docker-compose.yaml": "file-type-docker2",
  "docker-compose.yml": "file-type-docker2",
  dockerfile: "file-type-docker2",
  "eslint.config.js": "file-type-eslint",
  "eslint.config.mjs": "file-type-eslint",
  gemfile: "file-type-ruby",
  "go.mod": "file-type-go-package",
  "go.sum": "file-type-go-package",
  "go.work": "file-type-go-work",
  "jsconfig.json": "file-type-jsconfig",
  license: "file-type-license",
  makefile: DEFAULT_FILE_ICON,
  "package-lock.json": "file-type-npm",
  "package.json": "file-type-npm",
  "pnpm-lock.yaml": "file-type-pnpm",
  "pnpm-workspace.yaml": "file-type-pnpm",
  "svelte.config.js": "file-type-svelte",
  "tailwind.config.js": "file-type-tailwind",
  "tsconfig.json": "file-type-tsconfig",
  "vite.config.js": "file-type-vite",
  "vite.config.mjs": "file-type-vite",
  "vite.config.ts": "file-type-vite",
  "yarn.lock": "file-type-yarn",
};

const FILE_EXTENSIONS = {
  astro: "file-type-astro",
  bash: "file-type-shell",
  c: "file-type-c",
  cc: "file-type-cpp2",
  cjs: "file-type-js-official",
  clj: "file-type-clojure",
  cljs: "file-type-clojure",
  conf: "file-type-config",
  cpp: "file-type-cpp2",
  cs: "file-type-csharp2",
  css: "file-type-css",
  csv: "file-type-text",
  cxx: "file-type-cpp2",
  dart: "file-type-dartlang",
  diff: "file-type-diff",
  ex: "file-type-elixir",
  exs: "file-type-elixir",
  fish: "file-type-shell",
  fs: "file-type-fsharp",
  fsx: "file-type-fsharp",
  gif: "file-type-image",
  go: "file-type-go",
  gql: "file-type-graphql",
  graphql: "file-type-graphql",
  groovy: "file-type-groovy",
  gz: "file-type-zip",
  h: "file-type-cheader",
  hpp: "file-type-cppheader",
  htm: "file-type-html",
  html: "file-type-html",
  hs: "file-type-haskell",
  ini: "file-type-config",
  java: "file-type-java",
  jpeg: "file-type-image",
  jpg: "file-type-image",
  js: "file-type-js-official",
  json: "file-type-json",
  json5: "file-type-json",
  jsonc: "file-type-json",
  jsx: "file-type-reactjs",
  kt: "file-type-kotlin",
  kts: "file-type-kotlin",
  less: "file-type-less",
  log: "file-type-text",
  lua: "file-type-lua",
  md: "file-type-markdown",
  mdx: "file-type-markdown",
  mjs: "file-type-js-official",
  patch: "file-type-diff",
  pdf: "file-type-pdf2",
  php: "file-type-php3",
  pl: "file-type-perl",
  png: "file-type-image",
  proto: "file-type-protobuf",
  ps1: "file-type-powershell",
  py: "file-type-python",
  pyw: "file-type-python",
  r: "file-type-r",
  rb: "file-type-ruby",
  rs: "file-type-rust",
  sass: "file-type-sass",
  scala: "file-type-scala",
  scss: "file-type-scss2",
  sh: "file-type-shell",
  sql: "file-type-sql",
  svg: "file-type-svg",
  svelte: "file-type-svelte",
  swift: "file-type-swift",
  tar: "file-type-zip",
  toml: "file-type-toml",
  ts: "file-type-typescript-official",
  tsx: "file-type-reactts",
  txt: "file-type-text",
  vue: "file-type-vue",
  webp: "file-type-image",
  xml: "file-type-xml",
  yaml: "file-type-yaml",
  yml: "file-type-yaml",
  zig: "file-type-zig",
  zip: "file-type-zip",
  zsh: "file-type-shell",
};

const FOLDER_NAMES = {
  ".git": "folder-type-git",
  ".github": "folder-type-github",
  ".vscode": "folder-type-vscode",
  api: "folder-type-api",
  apis: "folder-type-api",
  app: "folder-type-app",
  assets: "folder-type-asset",
  bin: "folder-type-binary",
  build: "folder-type-dist",
  cmd: "folder-type-cli",
  component: "folder-type-component",
  components: "folder-type-component",
  conf: "folder-type-config",
  config: "folder-type-config",
  configs: "folder-type-config",
  coverage: "folder-type-coverage",
  css: "folder-type-css",
  data: "folder-type-db",
  database: "folder-type-db",
  db: "folder-type-db",
  dist: "folder-type-dist",
  doc: "folder-type-docs",
  docs: "folder-type-docs",
  images: "folder-type-images",
  lib: "folder-type-library",
  libs: "folder-type-library",
  node_modules: "folder-type-node",
  out: "folder-type-dist",
  packages: "folder-type-package",
  public: "folder-type-public",
  script: "folder-type-script",
  scripts: "folder-type-script",
  src: "folder-type-src",
  static: "folder-type-public",
  styles: "folder-type-css",
  target: "folder-type-dist",
  test: "folder-type-test",
  tests: "folder-type-test",
  types: "folder-type-typings",
  vendor: "folder-type-package",
};

function basename(name) {
  return String(name ?? "").replaceAll("\\", "/").split("/").pop().toLowerCase();
}

function resolveFileName(name) {
  if (FILE_NAMES[name]) return FILE_NAMES[name];
  if (name.startsWith(".env")) return "file-type-dotenv";
  if (name.startsWith("dockerfile")) return "file-type-docker2";
  if (/^license(?:\.|$)/.test(name)) return "file-type-license";
  if (/^readme(?:\.|$)/.test(name)) return "file-type-markdown";
  if (/^\.eslintrc\.(?:cjs|js|json|mjs|yaml|yml)$/.test(name)) return COMPOUND_FILE_ICONS.eslint;
  if (/^tsconfig\..+\.json$/.test(name)) return COMPOUND_FILE_ICONS.tsconfig;
  if (/\.d\.ts$/.test(name)) return COMPOUND_FILE_ICONS.typeScriptDefinition;
  if (/\.(?:test|spec)\.[cm]?jsx?$/.test(name)) return COMPOUND_FILE_ICONS.testJavaScript;
  if (/\.(?:test|spec)\.[cm]?tsx?$/.test(name)) return COMPOUND_FILE_ICONS.testTypeScript;
  if (/\.(?:test|spec)\.[^.]+$/.test(name)) return COMPOUND_FILE_ICONS.test;

  const dot = name.lastIndexOf(".");
  return dot > 0 && dot < name.length - 1
    ? (FILE_EXTENSIONS[name.slice(dot + 1)] ?? DEFAULT_FILE_ICON)
    : DEFAULT_FILE_ICON;
}

export function resolveFileIcon(name, { isDir = false, expanded = false } = {}) {
  const normalizedName = basename(name);
  let iconName;
  if (isDir) {
    iconName = FOLDER_NAMES[normalizedName] ?? DEFAULT_FOLDER_ICON;
    if (expanded) iconName += "-opened";
  } else {
    iconName = resolveFileName(normalizedName);
  }
  return `${PREFIX}:${iconName}`;
}

const folderIcons = Object.values(FOLDER_NAMES).flatMap((name) => [name, `${name}-opened`]);

export const FILE_ICON_NAMES = Object.freeze(
  [...new Set([
    DEFAULT_FILE_ICON,
    DEFAULT_FOLDER_ICON,
    `${DEFAULT_FOLDER_ICON}-opened`,
    ...Object.values(COMPOUND_FILE_ICONS),
    ...Object.values(FILE_NAMES),
    ...Object.values(FILE_EXTENSIONS),
    ...folderIcons,
  ])].sort(),
);
