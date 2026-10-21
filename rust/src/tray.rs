//! The tray owns a dedicated Win32 message thread, independent of egui painting.
use std::sync::{Arc, mpsc};
use std::time::Duration;
use tray_icon::menu::{CheckMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem};
use tray_icon::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use windows_sys::Win32::UI::WindowsAndMessaging::{
    DispatchMessageW, MSG, MsgWaitForMultipleObjects, PM_REMOVE, PeekMessageW, QS_ALLINPUT,
    TranslateMessage, WM_QUIT,
};

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Action {
    Ready,
    Show,
    Start,
    Stop,
    ToggleHud,
    Quit,
    Error(String),
}

#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub struct State {
    pub running: bool,
    pub busy: bool,
    pub hud: bool,
}

enum Command {
    Sync(State),
    Shutdown,
    #[cfg(test)]
    Inspect(mpsc::Sender<(isize, isize)>),
}

pub struct DesktopTray {
    commands: mpsc::Sender<Command>,
    thread: Option<std::thread::JoinHandle<()>>,
    last_state: Option<State>,
}

impl DesktopTray {
    pub fn new(handler: impl Fn(Action) + Send + Sync + 'static) -> Self {
        let handler: Arc<dyn Fn(Action) + Send + Sync> = Arc::new(handler);
        let (commands, receiver) = mpsc::channel();
        let thread = std::thread::spawn(move || {
            if let Err(error) = run(receiver, handler.clone()) {
                handler(Action::Error(error));
            }
        });
        Self {
            commands,
            thread: Some(thread),
            last_state: None,
        }
    }

    pub fn sync(&mut self, state: State) {
        if self.last_state != Some(state) {
            let _ = self.commands.send(Command::Sync(state));
            self.last_state = Some(state);
        }
    }
}

impl Drop for DesktopTray {
    fn drop(&mut self) {
        let _ = self.commands.send(Command::Shutdown);
        // Give the owning thread time to destroy its HWND and remove the shell icon.
        // Do not wait forever if Windows currently owns a modal popup menu loop.
        if let Some(thread) = self.thread.take() {
            for _ in 0..20 {
                if thread.is_finished() {
                    let _ = thread.join();
                    return;
                }
                std::thread::sleep(Duration::from_millis(10));
            }
        }
    }
}

fn run(
    receiver: mpsc::Receiver<Command>,
    handler: Arc<dyn Fn(Action) + Send + Sync>,
) -> Result<(), String> {
    let menu = Menu::new();
    let show = MenuItem::with_id("show", "显示主窗口", true, None);
    let hud = CheckMenuItem::with_id("hud", "显示 Token 悬浮窗", true, false, None);
    let start = MenuItem::with_id("start", "启动 API 服务", true, None);
    let stop = MenuItem::with_id("stop", "停止 API 服务", false, None);
    let quit = MenuItem::with_id("quit", "退出工作台", true, None);
    for item in [
        &show as &dyn tray_icon::menu::IsMenuItem,
        &hud,
        &PredefinedMenuItem::separator(),
        &start,
        &stop,
        &PredefinedMenuItem::separator(),
        &quit,
    ] {
        menu.append(item).map_err(|e| e.to_string())?;
    }
    let on_menu = handler.clone();
    MenuEvent::set_event_handler(Some(move |event: MenuEvent| {
        let action = match event.id.0.as_str() {
            "show" => Action::Show,
            "hud" => Action::ToggleHud,
            "start" => Action::Start,
            "stop" => Action::Stop,
            "quit" => Action::Quit,
            _ => return,
        };
        on_menu(action);
    }));
    let on_click = handler.clone();
    TrayIconEvent::set_event_handler(Some(move |event: TrayIconEvent| {
        if matches!(
            event,
            TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            }
        ) {
            on_click(Action::Show);
        }
    }));
    // Creating, dispatching and destroying all tray/menu HWNDs on this thread is essential.
    #[cfg(test)]
    let native_menu = tray_icon::menu::ContextMenu::hpopupmenu(&menu);
    let _tray = TrayIconBuilder::new()
        .with_id("workbuddy2api")
        .with_menu(Box::new(menu))
        .with_menu_on_left_click(false)
        .with_menu_on_right_click(true)
        .with_tooltip("WorkBuddy2API · 左键显示窗口 / 右键菜单")
        .with_icon(super::make_tray_icon())
        .build()
        .map_err(|e| e.to_string())?;
    handler(Action::Ready);
    let mut message: MSG = unsafe { std::mem::zeroed() };
    loop {
        // Win32 popup menu and icon callbacks must be dispatched even while egui is hidden.
        unsafe {
            while PeekMessageW(&mut message, std::ptr::null_mut(), 0, 0, PM_REMOVE) != 0 {
                if message.message == WM_QUIT {
                    return Ok(());
                }
                TranslateMessage(&message);
                DispatchMessageW(&message);
            }
        }
        loop {
            match receiver.try_recv() {
                #[cfg(test)]
                Ok(Command::Inspect(reply)) => {
                    let _ = reply.send((_tray.window_handle() as isize, native_menu));
                }
                Ok(Command::Sync(state)) => {
                    start.set_enabled(!state.busy && !state.running);
                    stop.set_enabled(!state.busy && state.running);
                    hud.set_checked(state.hud);
                    hud.set_text(if state.hud {
                        "隐藏 Token 悬浮窗"
                    } else {
                        "显示 Token 悬浮窗"
                    });
                }
                Ok(Command::Shutdown) | Err(mpsc::TryRecvError::Disconnected) => return Ok(()),
                Err(mpsc::TryRecvError::Empty) => break,
            }
        }
        unsafe {
            MsgWaitForMultipleObjects(0, std::ptr::null(), 0, 50, QS_ALLINPUT);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_tray_menu_dispatches_all_actions_and_exits_cleanly() {
        let (tx, rx) = mpsc::channel();
        let mut tray = DesktopTray::new(move |action| {
            let _ = tx.send(action);
        });
        assert_eq!(
            rx.recv_timeout(Duration::from_secs(3)).unwrap(),
            Action::Ready
        );
        use windows_sys::Win32::UI::WindowsAndMessaging::{
            GetMenuItemID, GetMenuState, IsWindowVisible, MF_BYPOSITION, MF_DISABLED, MF_GRAYED,
            PostMessageW, WM_COMMAND,
        };
        for (position, expected, running) in [
            (0, Action::Show, true),
            (1, Action::ToggleHud, true),
            (4, Action::Stop, true),
            (3, Action::Start, false),
            (6, Action::Quit, false),
        ] {
            tray.sync(State {
                running,
                busy: false,
                hud: false,
            });
            let (inspect_tx, inspect_rx) = mpsc::channel();
            tray.commands.send(Command::Inspect(inspect_tx)).unwrap();
            let (hwnd, menu) = inspect_rx.recv_timeout(Duration::from_secs(2)).unwrap();
            assert_eq!(unsafe { IsWindowVisible(hwnd as _) }, 0);
            let enabled = unsafe { GetMenuState(menu as _, position, MF_BYPOSITION) };
            assert_eq!(
                enabled & (MF_DISABLED | MF_GRAYED),
                0,
                "Tray item unexpectedly disabled"
            );
            let command_id = unsafe { GetMenuItemID(menu as _, position as i32) };
            assert_ne!(command_id, u32::MAX);
            assert_ne!(
                unsafe { PostMessageW(hwnd as _, WM_COMMAND, command_id as usize, 0) },
                0
            );
            assert_eq!(
                rx.recv_timeout(Duration::from_secs(2)).unwrap(),
                expected,
                "Native menu selection was lost while all tray HWNDs were hidden"
            );
        }
        tray.sync(State {
            running: true,
            busy: false,
            hud: true,
        });
        tray.sync(State::default());
        let thread = tray.thread.take().unwrap();
        tray.commands.send(Command::Shutdown).unwrap();
        for _ in 0..50 {
            if thread.is_finished() {
                thread.join().unwrap();
                return;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        panic!("Tray message thread did not exit and destroy its native icon");
    }
}
