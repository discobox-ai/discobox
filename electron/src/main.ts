import { app, BrowserWindow, Menu, ipcMain, protocol, shell } from "electron";
import { readFile } from "node:fs/promises";
import path from "node:path";

const APP_SCHEME = "app";
const DEFAULT_DEV_SERVER_URL = "http://localhost:5173";

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

async function createMainWindow(): Promise<BrowserWindow> {
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
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      preload: preloadPath(),
    },
  });

  window.on("ready-to-show", () => window.show());
  setupDevContextMenu(window);

  if (app.isPackaged) {
    await window.loadURL(`${APP_SCHEME}://discobox/`);
  } else {
    await window.loadURL(rendererDevURL());
  }

  return window;
}

app.on("window-all-closed", () => {
  if (process.platform !== "darwin") {
    app.quit();
  }
});

app.on("activate", async () => {
  if (BrowserWindow.getAllWindows().length === 0) {
    await createMainWindow();
  }
});

await app.whenReady();
await registerAppProtocol();
registerDesktopHandlers();
await createMainWindow();
