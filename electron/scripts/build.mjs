import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const __dirname = dirname(fileURLToPath(import.meta.url));
const electronRoot = dirname(__dirname);
const outputDir = join(electronRoot, "dist");

mkdirSync(outputDir, { recursive: true });

await Promise.all([
  build({
    entryPoints: {
      main: join(electronRoot, "src", "main.ts"),
    },
    outdir: outputDir,
    bundle: true,
    format: "esm",
    platform: "node",
    target: "node24",
    sourcemap: true,
    external: ["electron"],
  }),
  build({
    entryPoints: {
      preload: join(electronRoot, "src", "preload.ts"),
    },
    outdir: outputDir,
    bundle: true,
    format: "cjs",
    platform: "node",
    target: "node24",
    sourcemap: true,
    external: ["electron"],
  }),
]);
