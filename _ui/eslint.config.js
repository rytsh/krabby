import js from "@eslint/js";
import svelte from "eslint-plugin-svelte";
import globals from "globals";

import svelteConfig from "./svelte.config.js";

export default [
  {
    ignores: ["src/lib/file-icons.generated.js"],
  },
  js.configs.recommended,
  ...svelte.configs["flat/recommended"],
  {
    files: ["src/**/*.{js,svelte}"],
    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.es2025,
      },
    },
    rules: {
      "no-unused-vars": [
        "error",
        {
          argsIgnorePattern: "^_",
          caughtErrorsIgnorePattern: "^_",
        },
      ],
    },
  },
  {
    files: ["**/*.svelte"],
    languageOptions: {
      parserOptions: {
        svelteConfig,
      },
    },
  },
  {
    files: ["src/lib/CodeView.svelte"],
    rules: {
      // Shiki escapes source text and returns the trusted highlighted markup rendered here.
      "svelte/no-at-html-tags": "off",
    },
  },
  {
    files: ["src/lib/MarkdownView.svelte"],
    rules: {
      // Markdown HTML is allowlisted and sanitized with DOMPurify before rendering.
      "svelte/no-at-html-tags": "off",
    },
  },
  {
    files: ["src/lib/MermaidDiagram.svelte"],
    rules: {
      // svg-pan-zoom requires imperative ownership of the generated SVG subtree.
      "svelte/no-dom-manipulating": "off",
    },
  },
  {
    files: ["*.config.js", "scripts/**/*.mjs", "src/**/*.test.js"],
    languageOptions: {
      globals: globals.node,
    },
  },
];
