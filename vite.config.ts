import { defineConfig } from "vite-plus";

export default defineConfig({
  staged: {
    "*": "vp check --fix",
  },
  lint: {
    options: {
      typeAware: true,
      typeCheck: false,
    },
  },
  fmt: {
    sortImports: {},
    sortPackageJson: true,
    // Generated parity fixtures and the docs mirror the TypeScript reference
    // byte-for-byte; do not reformat them.
    ignorePatterns: ["internal/**/testdata/**", "docs/**"],
  },
});
