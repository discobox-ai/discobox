import { contextBridge, ipcRenderer } from "electron";

type DiscoboxDesktopBridge = {
  kind: "electron";
  windowMinimize: () => Promise<void>;
  windowMaximize: () => Promise<void>;
  windowUnmaximize: () => Promise<void>;
  windowIsMaximized: () => Promise<boolean>;
  windowClose: () => Promise<void>;
  openExternalUrl: (url: string) => Promise<void>;
};

const desktopAPI: DiscoboxDesktopBridge = {
  kind: "electron",
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
