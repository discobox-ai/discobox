//! Native notifications: a request that has just arrived (the requests topic's `arrived`) is
//! raised with the system's own notifications, and pressing one opens that request's card
//! (`Event::Answer`), which still asks for Accept. Which requests are new, and what a notification says, are the core's
//! (`console.RequestArrival`): the bridge sends none for what was waiting as the window opened,
//! which the list's marks and a workspace's notice say. Raising them is this window's platform
//! affordance (ADR 26-11-07-876 §2).
//!
//! On Linux it is the desktop's notification service over the session bus
//! (`org.freedesktop.Notifications`), with a default action that opens the request's card. A
//! session with no such service raises nothing, and the notice and the list say it still.
//! macOS and Windows raise nothing yet: neither builds the app (ADR 26-11-07-983 §4).

use crate::backend::Inbox;
use crate::wire::Arrival;

/// Raises a notification for a request that arrived; pressing it puts `Event::Answer` in the
/// inbox. Nothing here waits on the service: the call is made off the window's thread.
pub fn raise(inbox: &Inbox, arrival: &Arrival) { imp::raise(inbox, arrival) }

/// Text as the notification specification's body markup reads it literally: its body is a
/// subset of HTML on most servers (`<b>`, `<a href>`, `<img>`), and what a notification says of a
/// request is a box's own words — its title, the credential it named, its hosts. Escaped, a box
/// can neither style its notification nor hand it a link to open (cli/REVIEW.md: text a discobox
/// wrote is escaped for where it is shown). The summary is plain text by the specification.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub(crate) fn markup_escaped(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
    out
}

/// In a test nothing is raised: what would have been is kept, for the test to read.
#[cfg(test)]
mod imp {
    use super::*;

    thread_local! {
        pub static RAISED: std::cell::RefCell<Vec<Arrival>> = const { std::cell::RefCell::new(Vec::new()) };
    }

    pub fn raise(_: &Inbox, arrival: &Arrival) { RAISED.with_borrow_mut(|r| r.push(arrival.clone())); }
}

#[cfg(test)]
#[allow(unused_imports)]
pub use imp::RAISED;

#[cfg(all(not(test), target_os = "linux"))]
mod imp {
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex, OnceLock};

    use zbus::blocking::{Connection, Proxy};
    use zbus::zvariant::Value;

    use super::*;
    use crate::backend::Event;

    const SERVICE: &str = "org.freedesktop.Notifications";
    const PATH: &str = "/org/freedesktop/Notifications";
    /// The action a notification is pressed with, as the specification names its default.
    const DEFAULT: &str = "default";

    /// The session bus, and the notifications raised that are still up, by the ID the service
    /// gave each: what a press is matched against.
    struct Notifier {
        conn: Connection,
        raised: Mutex<HashMap<u32, Arrival>>,
    }

    static NOTIFIER: OnceLock<Option<Arc<Notifier>>> = OnceLock::new();

    /// The notifier, connected once, with a thread following the service's presses and closes:
    /// none for a session with no bus.
    fn notifier(inbox: &Inbox) -> Option<Arc<Notifier>> {
        NOTIFIER.get_or_init(|| {
            let conn = Connection::session().ok()?;
            let n = Arc::new(Notifier { conn, raised: Mutex::new(HashMap::new()) });
            let (follow, inbox) = (n.clone(), inbox.clone());
            std::thread::spawn(move || Notifier::follow(follow, &inbox));
            Some(n)
        }).clone()
    }

    impl Notifier {
        fn proxy(&self) -> zbus::Result<Proxy<'_>> { Proxy::new(&self.conn, SERVICE, PATH, SERVICE) }

        /// Takes a press on one of this window's notifications to the window, and forgets each
        /// once it closes.
        fn follow(n: Arc<Notifier>, inbox: &Inbox) {
            let closing = n.clone();
            std::thread::spawn(move || {
                let Ok(proxy) = closing.proxy() else { return };
                let Ok(closes) = proxy.receive_signal("NotificationClosed") else { return };
                for m in closes {
                    if let Ok((id, _)) = m.body().deserialize::<(u32, u32)>() {
                        closing.raised.lock().unwrap().remove(&id);
                    }
                }
            });
            let Ok(proxy) = n.proxy() else { return };
            let Ok(presses) = proxy.receive_signal("ActionInvoked") else { return };
            for m in presses {
                let Ok((id, action)) = m.body().deserialize::<(u32, String)>() else { continue };
                if action != DEFAULT {
                    continue;
                }
                if let Some(a) = n.raised.lock().unwrap().remove(&id) {
                    inbox.push(Event::Answer(a));
                }
            }
        }

        fn notify(&self, arrival: &Arrival) -> zbus::Result<u32> {
            let hints: HashMap<&str, Value> = HashMap::from([("urgency", Value::U8(1))]);
            let actions = vec![DEFAULT, "Answer"];
            let body = super::markup_escaped(&arrival.body);
            let reply = self.proxy()?.call_method("Notify", &("discobox", 0u32, "", arrival.title.as_str(), body.as_str(), actions, hints, -1i32))?;
            reply.body().deserialize::<u32>()
        }
    }

    pub fn raise(inbox: &Inbox, arrival: &Arrival) {
        let (inbox, arrival) = (inbox.clone(), arrival.clone());
        std::thread::spawn(move || {
            let Some(n) = notifier(&inbox) else { return };
            if let Ok(id) = n.notify(&arrival) {
                n.raised.lock().unwrap().insert(id, arrival);
            }
        });
    }
}

#[cfg(all(not(test), not(target_os = "linux")))]
mod imp {
    use super::*;

    pub fn raise(_: &Inbox, _: &Arrival) {}
}
