import { cpSync, existsSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

const __dirname = dirname(fileURLToPath(import.meta.url));
const electronRoot = dirname(__dirname);
const workspaceRoot = dirname(electronRoot);
const uiRoot = join(workspaceRoot, "ui");
const uiBuildDir = join(uiRoot, "build");
const packagedUiDir = join(electronRoot, "dist", "ui");

execFileSync("pnpm", ["--dir", uiRoot, "install", "--frozen-lockfile"], {
  stdio: "inherit",
});
execFileSync("pnpm", ["--dir", uiRoot, "run", "build"], { stdio: "inherit" });

if (!existsSync(uiBuildDir)) {
  throw new Error(`UI build output was not created at ${uiBuildDir}`);
}

rmSync(packagedUiDir, { recursive: true, force: true });
cpSync(uiBuildDir, packagedUiDir, { recursive: true });
