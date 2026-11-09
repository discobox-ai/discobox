// Package console is the console's core: what the console means, shared by
// every frontend that draws it — the TUI (`cli/internal/tui`) and the desktop
// app (ADR 26-11-07-876). It owns the data seam, DataSource and the types it
// carries, which the CLI implements against the server; the command registry
// every frontend dispatches through; and the forms (Form) every question the
// console asks is described as, with their validation. It imports no frontend.
package console
