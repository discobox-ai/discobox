import { app, BrowserWindow, Menu, ipcMain, protocol, shell } from "electron";
import { spawn, type ChildProcessByStdio } from "node:child_process";
import type { Readable } from "node:stream";
import { readFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

const APP_SCHEME = "app";
const DEFAULT_DEV_SERVER_URL = "http://localhost:5173";
const TITLE_BAR_HEIGHT = 40;
const IS_DEV = !app.isPackaged;
let serverProcess: ChildProcessByStdio<null, Readable, Readable> | null = null;
type WindowControlsMode =
  | "macos"
  | "windows"
  | "linux"
  | "none"
  | "macos-fullscreen";

protocol.registerSchemesAsPrivileged([
  {
    scheme: APP_SCHEME,
    privileges: {
      standard: true,
      secure: true,
      supportFetchAPI: true,
      corsEnabled: true,
    },
  },
]);

function rendererDevURL(): string {
  return process.env.DISCOBOX_UI_DEV_URL || DEFAULT_DEV_SERVER_URL;
}

function devLog(message: string): void {
  if (IS_DEV) {
    console.log(`[discobox-electron] ${message}`);
  }
}

function appLog(message: string): void {
  console.log(`[discobox-electron] ${message}`);
}

function defaultServerEndpoint(): string {
  if (process.platform === "win32") {
    return "npipe:////./pipe/discobox";
  }

  const runtimeDir =
    process.env.XDG_RUNTIME_DIR ||
    path.join(os.tmpdir(), `discobox-${os.userInfo().uid}`);
  return `unix://${path.join(runtimeDir, "discobox", "server.sock")}`;
}

function packagedServerPath(): string {
  const binaryName =
    process.platform === "win32" ? "discobox-server.exe" : "discobox-server";
  if (app.isPackaged) {
    return path.join(
      process.resourcesPath,
      "app.asar.unpacked",
      "dist",
      "bin",
      binaryName,
    );
  }
  return path.resolve(import.meta.dirname, "bin", binaryName);
}

function startPackagedServer(): void {
  if (IS_DEV || serverProcess) {
    return;
  }

  const endpoint = process.env.DISCOBOX_SERVER || defaultServerEndpoint();
  const userDataDir = app.getPath("userData");
  const env = {
    ...process.env,
    DISCOBOX_SERVER_LISTEN: endpoint,
    DISCOBOX_SERVER: endpoint,
    DISCOBOX_DATA_DIR:
      process.env.DISCOBOX_DATA_DIR || path.join(userDataDir, "data"),
    DISCOBOX_CONFIG_DIR:
      process.env.DISCOBOX_CONFIG_DIR || path.join(userDataDir, "config"),
    DISCOBOX_CACHE_DIR:
      process.env.DISCOBOX_CACHE_DIR || path.join(userDataDir, "cache"),
    DISCOBOX_STATE_DIR:
      process.env.DISCOBOX_STATE_DIR || path.join(userDataDir, "state"),
  };

  const binaryPath = packagedServerPath();
  appLog(`starting server ${binaryPath} on ${endpoint}`);
  const child = spawn(binaryPath, [], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  serverProcess = child;
  child.stdout.on("data", (chunk: Buffer) => {
    process.stdout.write(`[discobox-server] ${chunk.toString()}`);
  });
  child.stderr.on("data", (chunk: Buffer) => {
    process.stderr.write(`[discobox-server] ${chunk.toString()}`);
  });
  child.on("exit", (code, signal) => {
    appLog(`server exited code=${code ?? "null"} signal=${signal ?? "null"}`);
    serverProcess = null;
  });
  child.on("error", (error) => {
    console.error("[discobox-electron] failed to start server", error);
    serverProcess = null;
  });
}

function stopPackagedServer(): void {
  if (!serverProcess) {
    return;
  }
  serverProcess.kill();
  serverProcess = null;
}

function uiBuildDir(): string {
  return path.resolve(import.meta.dirname, "ui");
}

function resolveAppAssetPath(requestURL: string): string | null {
  const requestedPath = decodeURIComponent(new URL(requestURL).pathname);
  const relativePath =
    requestedPath === "/" ? "index.html" : requestedPath.replace(/^\/+/, "");
  const buildDir = uiBuildDir();
  const filePath = path.normalize(path.join(buildDir, relativePath));
  const relative = path.relative(buildDir, filePath);

  if (relative.startsWith("..") || path.isAbsolute(relative)) {
    return null;
  }

  return filePath;
}

function contentTypeForAppAsset(filePath: string): string {
  switch (path.extname(filePath).toLowerCase()) {
    case ".html":
      return "text/html; charset=utf-8";
    case ".js":
      return "text/javascript; charset=utf-8";
    case ".css":
      return "text/css; charset=utf-8";
    case ".json":
      return "application/json; charset=utf-8";
    case ".svg":
      return "image/svg+xml";
    case ".png":
      return "image/png";
    case ".jpg":
    case ".jpeg":
      return "image/jpeg";
    case ".woff":
      return "font/woff";
    case ".woff2":
      return "font/woff2";
    default:
      return "application/octet-stream";
  }
}

async function registerAppProtocol(): Promise<void> {
  protocol.handle(APP_SCHEME, async (request) => {
    const filePath = resolveAppAssetPath(request.url);
    if (!filePath) {
      return new Response("Not found", { status: 404 });
    }

    try {
      const asset = await readFile(filePath);
      return new Response(asset, {
        headers: { "content-type": contentTypeForAppAsset(filePath) },
      });
    } catch {
      return new Response("Not found", { status: 404 });
    }
  });
}

function currentWindow(event: Electron.IpcMainInvokeEvent): BrowserWindow {
  const window = BrowserWindow.fromWebContents(event.sender);
  if (!window) {
    throw new Error("Could not resolve the current Electron window");
  }
  return window;
}

function windowControlsMode(window?: BrowserWindow): WindowControlsMode {
  switch (process.platform) {
    case "darwin":
      return window?.isFullScreen() ? "macos-fullscreen" : "macos";
    case "win32":
      return "windows";
    case "linux":
      return "linux";
    default:
      return "none";
  }
}

function notifyWindowControlsMode(window: BrowserWindow): void {
  window.webContents.send(
    "desktop:window-controls-changed",
    windowControlsMode(window),
  );
}

function preloadPath(): string {
  return path.resolve(import.meta.dirname, "preload.js");
}

function safeExternalURL(rawURL: unknown): string {
  if (typeof rawURL !== "string") {
    throw new Error("External URL must be a string");
  }

  const url = new URL(rawURL);
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("External URL protocol must be http or https");
  }

  return url.href;
}

function registerDesktopHandlers(): void {
  ipcMain.handle("desktop:window-minimize", (event) =>
    currentWindow(event).minimize(),
  );
  ipcMain.handle("desktop:window-maximize", (event) =>
    currentWindow(event).maximize(),
  );
  ipcMain.handle("desktop:window-unmaximize", (event) =>
    currentWindow(event).unmaximize(),
  );
  ipcMain.handle("desktop:window-is-maximized", (event) =>
    currentWindow(event).isMaximized(),
  );
  ipcMain.handle("desktop:window-controls", (event) =>
    windowControlsMode(currentWindow(event)),
  );
  ipcMain.handle("desktop:window-close", (event) =>
    currentWindow(event).close(),
  );
  ipcMain.handle("desktop:open-external", (_event, url: unknown) =>
    shell.openExternal(safeExternalURL(url)),
  );
}

function setupDevContextMenu(window: BrowserWindow): void {
  window.webContents.on("context-menu", (_event, params) => {
    const menu = Menu.buildFromTemplate([
      {
        label: "Reload",
        click: () => window.webContents.reload(),
      },
      {
        label: "Inspect Element",
        click: () => {
          window.webContents.inspectElement(params.x, params.y);
          if (!window.webContents.isDevToolsOpened()) {
            window.webContents.openDevTools({ mode: "detach" });
          }
        },
      },
    ]);
    menu.popup({ window });
  });
}

function revealWindow(window: BrowserWindow): void {
  if (window.isDestroyed() || window.isVisible()) {
    return;
  }

  window.show();
  window.focus();
}

async function createMainWindow(): Promise<BrowserWindow> {
  devLog("creating main window");

  const nativeTitleBarOverlay =
    process.platform === "darwin"
      ? {}
      : {
          frame: false,
          titleBarOverlay: {
            color: "#0b0b0d",
            symbolColor: "#f4f4f5",
            height: TITLE_BAR_HEIGHT,
          },
        };

  const window = new BrowserWindow({
    width: 1440,
    height: 960,
    minWidth: 960,
    minHeight: 640,
    show: false,
    autoHideMenuBar: true,
    title: "Discobox",
    backgroundColor: "#0b0b0d",
    titleBarStyle: process.platform === "darwin" ? "hiddenInset" : "hidden",
    frame: process.platform === "darwin",
    ...nativeTitleBarOverlay,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      preload: preloadPath(),
    },
  });

  window.on("ready-to-show", () => revealWindow(window));
  window.webContents.on("did-finish-load", () => revealWindow(window));
  setTimeout(() => revealWindow(window), 3000);
  window.on("enter-full-screen", () => notifyWindowControlsMode(window));
  window.on("leave-full-screen", () => notifyWindowControlsMode(window));
  setupDevContextMenu(window);

  if (app.isPackaged) {
    await window.loadURL(`${APP_SCHEME}://discobox/`);
  } else {
    const devURL = rendererDevURL();
    devLog(`loading renderer from ${devURL}`);
    await window.loadURL(devURL);
  }

  devLog("main window loaded");
  return window;
}

app.on("window-all-closed", () => {
  if (process.platform !== "darwin") {
    app.quit();
  }
});

app.on("before-quit", () => {
  stopPackagedServer();
});

app.on("activate", async () => {
  if (BrowserWindow.getAllWindows().length === 0) {
    await createMainWindow();
  }
});

devLog("waiting for Electron app readiness");
void app
  .whenReady()
  .then(async () => {
    devLog("Electron app is ready");
    startPackagedServer();
    await registerAppProtocol();
    registerDesktopHandlers();
    await createMainWindow();
  })
  .catch((error: unknown) => {
    console.error("[discobox-electron] failed to start", error);
    app.quit();
  });
