import { mkdirSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const electronRoot = dirname(__dirname);
const workspaceRoot = dirname(electronRoot);
const outputDir = join(electronRoot, "dist", "bin");

mkdirSync(outputDir, { recursive: true });

const binaryName =
  process.platform === "win32" ? "discobox-server.exe" : "discobox-server";

execFileSync(
  "go",
  ["build", "-o", join(outputDir, binaryName), "./cmd/discobox-server"],
  {
    cwd: join(workspaceRoot, "server"),
    stdio: "inherit",
  },
);
