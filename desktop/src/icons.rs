//! The window's icons: vector strokes on a 24-unit grid, tinted by the element that draws them.
//! They stand where the TUI types characters: `[+]` and `[x]`, the sidebar's arrows, the
//! cursor chevron, a nested box's elbow.

use rune::Icon;

macro_rules! stroked {
    ($name:ident, $id:literal, $body:literal) => {
        pub static $name: Icon = Icon {
            name: concat!("discobox-", $id),
            view: 24.0,
            body: concat!(
                r#"<path d=""#, $body,
                r#"" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>"#
            ),
        };
    };
}

stroked!(PLUS, "plus", "M12 5v14M5 12h14");
stroked!(CLOSE, "close", "M7 7l10 10M17 7L7 17");
stroked!(MAXIMIZE, "maximize", "M4 9V4h5M15 4h5v5M20 15v5h-5M9 20H4v-5");
stroked!(RESTORE, "restore", "M9 4v5H4M20 9h-5V4M15 20v-5h5M4 15h5v5");
stroked!(HIDE_SIDEBAR, "hide-sidebar", "M15 6l-6 6 6 6");
stroked!(SHOW_SIDEBAR, "show-sidebar", "M9 6l6 6-6 6");
stroked!(CHEVRON, "chevron", "M9 6l6 6-6 6");
stroked!(SEARCH, "search", "M10.5 4a6.5 6.5 0 1 0 0 13a6.5 6.5 0 1 0 0-13M15.5 15.5L20 20");
stroked!(NESTED, "nested", "M7 4v7a2 2 0 0 0 2 2h8");
stroked!(BOX, "box", "M7 4h10a3 3 0 0 1 3 3v10a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3V7a3 3 0 0 1 3-3");
stroked!(COMMAND, "command", "M5 7l5 5-5 5M13 17h6");

pub static MORE: Icon = Icon {
    name: "discobox-more",
    view: 24.0,
    body: r#"<circle cx="5" cy="12" r="1.8" fill="currentColor"/><circle cx="12" cy="12" r="1.8" fill="currentColor"/><circle cx="19" cy="12" r="1.8" fill="currentColor"/>"#,
};
