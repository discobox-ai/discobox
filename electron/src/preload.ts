import { contextBridge, ipcRenderer } from "electron";

type WindowControlsMode =
  | "macos"
  | "windows"
  | "linux"
  | "none"
  | "macos-fullscreen";
type AppPlatform = "macos" | "windows" | "linux" | "web" | "unknown";

type DiscoboxDesktopBridge = {
  kind: "electron";
  platform: AppPlatform;
  windowControls: WindowControlsMode;
  getWindowControls: () => Promise<WindowControlsMode>;
  onWindowControlsChange: (
    callback: (windowControls: WindowControlsMode) => void,
  ) => () => void;
  windowMinimize: () => Promise<void>;
  windowMaximize: () => Promise<void>;
  windowUnmaximize: () => Promise<void>;
  windowIsMaximized: () => Promise<boolean>;
  windowClose: () => Promise<void>;
  openExternalUrl: (url: string) => Promise<void>;
};

function appPlatform(): AppPlatform {
  switch (process.platform) {
    case "darwin":
      return "macos";
    case "win32":
      return "windows";
    case "linux":
      return "linux";
    default:
      return "unknown";
  }
}

function windowControlsMode(): WindowControlsMode {
  switch (process.platform) {
    case "darwin":
      return "macos";
    case "win32":
      return "windows";
    case "linux":
      return "linux";
    default:
      return "none";
  }
}

const desktopAPI: DiscoboxDesktopBridge = {
  kind: "electron",
  platform: appPlatform(),
  windowControls: windowControlsMode(),
  getWindowControls: () =>
    ipcRenderer.invoke(
      "desktop:window-controls",
    ) as Promise<WindowControlsMode>,
  onWindowControlsChange: (callback) => {
    const listener = (
      _event: Electron.IpcRendererEvent,
      mode: WindowControlsMode,
    ) => {
      callback(mode);
    };

    ipcRenderer.on("desktop:window-controls-changed", listener);
    return () =>
      ipcRenderer.removeListener("desktop:window-controls-changed", listener);
  },
  windowMinimize: () =>
    ipcRenderer.invoke("desktop:window-minimize") as Promise<void>,
  windowMaximize: () =>
    ipcRenderer.invoke("desktop:window-maximize") as Promise<void>,
  windowUnmaximize: () =>
    ipcRenderer.invoke("desktop:window-unmaximize") as Promise<void>,
  windowIsMaximized: () =>
    ipcRenderer.invoke("desktop:window-is-maximized") as Promise<boolean>,
  windowClose: () =>
    ipcRenderer.invoke("desktop:window-close") as Promise<void>,
  openExternalUrl: (url) =>
    ipcRenderer.invoke("desktop:open-external", url) as Promise<void>,
};

contextBridge.exposeInMainWorld("__DISCOBOX_DESKTOP__", desktopAPI);
