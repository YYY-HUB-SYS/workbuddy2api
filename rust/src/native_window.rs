//! Main-window chrome and wakeups, including when Win32 suppresses hidden redraws.
pub const TITLE: &str = "WorkBuddy2API";

#[derive(Clone, Copy, Default)]
pub struct MainWindow(isize);

impl MainWindow {
    pub fn from_context(cc: &eframe::CreationContext<'_>) -> Self {
        use raw_window_handle::{HasWindowHandle, RawWindowHandle};
        if let Ok(handle) = cc.window_handle() {
            if let RawWindowHandle::Win32(handle) = handle.as_raw() {
                return Self(handle.hwnd.get());
            }
        }
        Self::default()
    }

    pub fn wake(self) {
        if self.0 != 0 {
            // request_redraw alone does not produce WM_PAINT for an invisible window.
            // Posting it explicitly reaches winit's RedrawRequested path without showing it.
            unsafe {
                windows_sys::Win32::UI::WindowsAndMessaging::PostMessageW(
                    self.0 as _,
                    windows_sys::Win32::UI::WindowsAndMessaging::WM_PAINT,
                    0,
                    0,
                );
            }
        }
    }

    pub fn dark_title_bar(self) -> bool {
        use windows_sys::Win32::Graphics::Dwm::*;
        if self.0 == 0 {
            return false;
        }
        let dark: i32 = 1;
        let set = |attribute: i32, value: &u32| unsafe {
            DwmSetWindowAttribute(
                self.0 as _,
                attribute as u32,
                value as *const u32 as _,
                std::mem::size_of::<u32>() as u32,
            )
        };
        let accepted =
            set(DWMWA_USE_IMMERSIVE_DARK_MODE, &(dark as u32)) >= 0 || set(19, &(dark as u32)) >= 0; // Older Windows 10 builds.
        // Windows 11 supports explicit colors; Windows 10 uses its native dark caption.
        let _ = set(DWMWA_CAPTION_COLOR, &0x0018_1514);
        let _ = set(DWMWA_TEXT_COLOR, &0x00F1_EDEC);
        accepted
    }
}

const INSTANCE_NAME: &str = "Local\\WorkBuddy2API.Desktop";
const QUIT_NAME: &str = "Local\\WorkBuddy2API.Desktop.Quit";

pub struct Instance {
    mutex: windows_sys::Win32::Foundation::HANDLE,
    quit_event: windows_sys::Win32::Foundation::HANDLE,
}

impl Instance {
    /// Hold a session-wide mutex for the entire GUI lifetime, including other copies of the exe.
    pub fn acquire() -> std::io::Result<Option<Self>> {
        Self::acquire_named(INSTANCE_NAME)
    }

    fn acquire_named(name: &str) -> std::io::Result<Option<Self>> {
        use windows_sys::Win32::Foundation::{CloseHandle, ERROR_ALREADY_EXISTS, GetLastError};
        use windows_sys::Win32::System::Threading::CreateMutexW;
        let event_name: Vec<u16> = format!("{name}.Quit\0").encode_utf16().collect();
        let name: Vec<u16> = name.encode_utf16().chain(Some(0)).collect();
        let handle = unsafe { CreateMutexW(std::ptr::null(), 0, name.as_ptr()) };
        if handle.is_null() {
            return Err(std::io::Error::last_os_error());
        }
        if unsafe { GetLastError() } == ERROR_ALREADY_EXISTS {
            unsafe {
                CloseHandle(handle);
            }
            Ok(None)
        } else {
            let quit_event = unsafe { windows_sys::Win32::System::Threading::CreateEventW(
                std::ptr::null(), 0, 0, event_name.as_ptr()) };
            if quit_event.is_null() {
                let error = std::io::Error::last_os_error();
                unsafe { CloseHandle(handle); }
                return Err(error);
            }
            Ok(Some(Self { mutex: handle, quit_event }))
        }
    }
}

impl Drop for Instance {
    fn drop(&mut self) {
        unsafe {
            windows_sys::Win32::Foundation::CloseHandle(self.quit_event);
            windows_sys::Win32::Foundation::CloseHandle(self.mutex);
        }
    }
}

pub fn quit_requested() -> bool {
    use windows_sys::Win32::Foundation::CloseHandle;
    use windows_sys::Win32::System::Threading::*;
    let name: Vec<u16> = QUIT_NAME.encode_utf16().chain(Some(0)).collect();
    unsafe {
        let event = OpenEventW(SYNCHRONIZATION_SYNCHRONIZE, 0, name.as_ptr());
        if event.is_null() { return false; }
        let requested = WaitForSingleObject(event, 0) == 0;
        CloseHandle(event);
        requested
    }
}

pub fn request_exit() -> std::io::Result<()> {
    use windows_sys::Win32::Foundation::CloseHandle;
    use windows_sys::Win32::System::Threading::*;
    let name: Vec<u16> = QUIT_NAME.encode_utf16().chain(Some(0)).collect();
    unsafe {
        let event = OpenEventW(EVENT_MODIFY_STATE, 0, name.as_ptr());
        if event.is_null() { return Ok(()); }
        let signalled = SetEvent(event) != 0;
        CloseHandle(event);
        if !signalled { return Err(std::io::Error::last_os_error()); }
    }
    // Wait until the GUI releases its event, so installers can replace the executable safely.
    let started = std::time::Instant::now();
    while started.elapsed() < std::time::Duration::from_secs(15) {
        unsafe {
            let event = OpenEventW(SYNCHRONIZATION_SYNCHRONIZE, 0, name.as_ptr());
            if event.is_null() { return Ok(()); }
            CloseHandle(event);
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    Err(std::io::Error::new(std::io::ErrorKind::TimedOut, "工作台尚未退出"))
}

pub fn duplicate_notice() {
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        MB_ICONINFORMATION, MB_OK, MB_SETFOREGROUND, MessageBoxW,
    };
    let title: Vec<u16> = "WorkBuddy2API 已在运行\0".encode_utf16().collect();
    let text: Vec<u16> = "已经启动了一个 WorkBuddy2API 工作台。\n\n如果主界面已收起，请在系统托盘区找到 WorkBuddy2API 图标，选择“显示主窗口”。\n无需重复启动。\0".encode_utf16().collect();
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            text.as_ptr(),
            title.as_ptr(),
            MB_OK | MB_ICONINFORMATION | MB_SETFOREGROUND,
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use windows_sys::Win32::UI::WindowsAndMessaging::*;

    #[test]
    fn only_one_gui_instance_can_hold_the_portable_instance_lock() {
        let name = format!("Local\\WorkBuddy2API.Test-{}", std::process::id());
        let first = Instance::acquire_named(&name)
            .unwrap()
            .expect("first instance must acquire lock");
        assert!(
            Instance::acquire_named(&name).unwrap().is_none(),
            "duplicate GUI must not start"
        );
        drop(first);
        assert!(
            Instance::acquire_named(&name).unwrap().is_some(),
            "exiting must release the instance lock"
        );
    }

    #[test]
    fn hidden_main_window_receives_a_wakeup_without_becoming_visible() {
        let class: Vec<u16> = "STATIC\0".encode_utf16().collect();
        let title: Vec<u16> = "WorkBuddy2API hidden test\0".encode_utf16().collect();
        let hwnd = unsafe {
            CreateWindowExW(
                0,
                class.as_ptr(),
                title.as_ptr(),
                WS_OVERLAPPEDWINDOW,
                0,
                0,
                300,
                200,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null(),
            )
        };
        assert!(!hwnd.is_null());
        let window = MainWindow(hwnd as isize);
        assert!(
            window.dark_title_bar(),
            "Windows rejected the native dark caption"
        );
        window.wake();
        let mut message: MSG = unsafe { std::mem::zeroed() };
        assert_ne!(
            unsafe { PeekMessageW(&mut message, hwnd, WM_PAINT, WM_PAINT, PM_REMOVE) },
            0,
            "Tray commands must trigger a frame even for a hidden main window"
        );
        assert_eq!(
            unsafe { IsWindowVisible(hwnd) },
            0,
            "A background wakeup must not show the window"
        );
        let mut dark = 0u32;
        let result = unsafe {
            windows_sys::Win32::Graphics::Dwm::DwmGetWindowAttribute(
                hwnd,
                windows_sys::Win32::Graphics::Dwm::DWMWA_USE_IMMERSIVE_DARK_MODE as u32,
                &mut dark as *mut u32 as _,
                4,
            )
        };
        if result >= 0 {
            assert_eq!(dark, 1);
        }
        unsafe {
            DestroyWindow(hwnd);
        }
    }
}
