//! Clip the Windows HUD at the compositor level, independent of OpenGL alpha support.
pub const TITLE: &str = "Token HUD";

pub fn sync_window_shape() -> Result<(), String> {
    #[cfg(target_os = "windows")]
    windows::sync()?;
    Ok(())
}

#[cfg(target_os = "windows")]
mod windows {
    use super::TITLE;
    use crate::{HUD_GUTTER, HUD_H, HUD_RADIUS, HUD_W};
    use std::ptr::null_mut;
    use windows_sys::Win32::Foundation::{HWND, LPARAM, POINT, RECT};
    use windows_sys::Win32::Graphics::Gdi::{
        ClientToScreen, CreateRectRgn, CreateRoundRectRgn, DeleteObject, EqualRgn, GetWindowRgn,
        HRGN, SetWindowRgn,
    };
    use windows_sys::Win32::System::Threading::GetCurrentThreadId;
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        EnumThreadWindows, GetClientRect, GetWindowRect, GetWindowTextW,
    };

    struct Region(HRGN);
    impl Drop for Region {
        fn drop(&mut self) {
            unsafe {
                DeleteObject(self.0);
            }
        }
    }

    unsafe extern "system" fn find_hud(window: HWND, parameter: LPARAM) -> i32 {
        let mut title = [0_u16; 64];
        let length = unsafe { GetWindowTextW(window, title.as_mut_ptr(), title.len() as i32) };
        if length > 0
            && title[..length as usize]
                .iter()
                .copied()
                .eq(TITLE.encode_utf16())
        {
            // EnumThreadWindows invokes this synchronously; the pointer belongs to sync().
            unsafe {
                *(parameter as *mut HWND) = window;
            }
            return 0;
        }
        1
    }

    pub(super) fn sync() -> Result<(), String> {
        let mut window: HWND = null_mut();
        // Scope to our GUI thread. Other instances, the tray thread and other apps are excluded.
        unsafe {
            EnumThreadWindows(
                GetCurrentThreadId(),
                Some(find_hud),
                &mut window as *mut HWND as LPARAM,
            );
        }
        if window.is_null() {
            // Deferred viewports may not have an HWND yet on their first pass.
            return Ok(());
        }
        apply(window)
    }

    fn apply(window: HWND) -> Result<(), String> {
        let mut client = RECT::default();
        let mut outer = RECT::default();
        let mut origin = POINT::default();
        unsafe {
            if GetClientRect(window, &mut client) == 0
                || GetWindowRect(window, &mut outer) == 0
                || ClientToScreen(window, &mut origin) == 0
            {
                return Err(std::io::Error::last_os_error().to_string());
            }
        }
        let width = client.right - client.left;
        let height = client.bottom - client.top;
        if width <= 0 || height <= 0 {
            return Ok(());
        }
        // Use the actual physical client size, including per-monitor DPI changes.
        let sx = width as f32 / HUD_W;
        let sy = height as f32 / HUD_H;
        let gx = (HUD_GUTTER * sx).round() as i32;
        let gy = (HUD_GUTTER * sy).round() as i32;
        let x = origin.x - outer.left;
        let y = origin.y - outer.top;
        let desired = Region(unsafe {
            // GDI excludes the final coordinate; +1 preserves the full last pixel row/column.
            CreateRoundRectRgn(
                x + gx,
                y + gy,
                x + width - gx + 1,
                y + height - gy + 1,
                (HUD_RADIUS * 2.0 * sx).round() as i32,
                (HUD_RADIUS * 2.0 * sy).round() as i32,
            )
        });
        if desired.0.is_null() {
            return Err("无法创建原生圆角区域".into());
        }
        let current = Region(unsafe { CreateRectRgn(0, 0, 0, 0) });
        if current.0.is_null() {
            return Err("无法读取原生窗口区域".into());
        }
        // No cache keyed only by HWND: Windows can reuse that handle after the HUD is reopened.
        if unsafe { GetWindowRgn(window, current.0) } != 0
            && unsafe { EqualRgn(current.0, desired.0) } != 0
        {
            return Ok(());
        }
        if unsafe { SetWindowRgn(window, desired.0, 1) } == 0 {
            return Err("无法应用原生圆角区域".into());
        }
        // SetWindowRgn transfers ownership to Windows. Only failed/unused regions are deleted here.
        std::mem::forget(desired);
        Ok(())
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use windows_sys::Win32::Graphics::Gdi::{GetRgnBox, PtInRegion};
        use windows_sys::Win32::UI::WindowsAndMessaging::{
            CreateWindowExW, DestroyWindow, SWP_NOACTIVATE, SWP_NOMOVE, SWP_NOZORDER, SetWindowPos,
            WS_POPUP,
        };

        struct TestWindow(HWND);
        impl TestWindow {
            fn new(title: &str, width: i32, height: i32) -> Self {
                let class: Vec<u16> = "STATIC\0".encode_utf16().collect();
                let title: Vec<u16> = title.encode_utf16().chain(Some(0)).collect();
                let window = unsafe {
                    CreateWindowExW(
                        0,
                        class.as_ptr(),
                        title.as_ptr(),
                        WS_POPUP,
                        0,
                        0,
                        width,
                        height,
                        null_mut(),
                        null_mut(),
                        null_mut(),
                        std::ptr::null(),
                    )
                };
                assert!(
                    !window.is_null(),
                    "Failed to create hidden native test window"
                );
                Self(window)
            }
        }
        impl Drop for TestWindow {
            fn drop(&mut self) {
                unsafe {
                    DestroyWindow(self.0);
                }
            }
        }

        #[test]
        fn native_hud_region_removes_gutter_and_corners_after_resize_and_reopen() {
            // Real Win32 windows and GDI regions, hidden throughout; no account or gateway access.
            let untouched = TestWindow::new("WorkBuddy2API test main window", 400, 300);
            for _reopen in 0..2 {
                let hud = TestWindow::new(TITLE, HUD_W as i32, HUD_H as i32);
                for scale in [1.0_f32, 1.25, 1.5, 2.0] {
                    let w = (HUD_W * scale).round() as i32;
                    let h = (HUD_H * scale).round() as i32;
                    unsafe {
                        SetWindowPos(
                            hud.0,
                            null_mut(),
                            0,
                            0,
                            w,
                            h,
                            SWP_NOMOVE | SWP_NOZORDER | SWP_NOACTIVATE,
                        );
                    }
                    sync().unwrap();
                    sync().unwrap(); // Repeated paint must keep the same correct region.
                    let region = Region(unsafe { CreateRectRgn(0, 0, 0, 0) });
                    assert_ne!(unsafe { GetWindowRgn(hud.0, region.0) }, 0);
                    let mut bounds = RECT::default();
                    unsafe {
                        GetRgnBox(region.0, &mut bounds);
                    }
                    let gx = (HUD_GUTTER * w as f32 / HUD_W).round() as i32;
                    let gy = (HUD_GUTTER * h as f32 / HUD_H).round() as i32;
                    assert_eq!(
                        (bounds.left, bounds.top, bounds.right, bounds.bottom),
                        (gx, gy, w - gx, h - gy),
                        "Incorrect physical clipping at scale {scale}"
                    );
                    for x in 0..w {
                        for y in 0..h {
                            if x < gx || x >= w - gx || y < gy || y >= h - gy {
                                assert_eq!(
                                    unsafe { PtInRegion(region.0, x, y) },
                                    0,
                                    "The black gutter remains in the window at {x}, {y}"
                                );
                            }
                        }
                    }
                    for (x, y) in [
                        (gx, gy),
                        (w - gx - 1, gy),
                        (gx, h - gy - 1),
                        (w - gx - 1, h - gy - 1),
                    ] {
                        assert_eq!(
                            unsafe { PtInRegion(region.0, x, y) },
                            0,
                            "Square corner remains"
                        );
                    }
                    for (x, y) in [
                        (w / 2, h / 2),
                        (gx, h / 2),
                        (w - gx - 1, h / 2),
                        (w / 2, gy),
                        (w / 2, h - gy - 1),
                    ] {
                        assert_ne!(
                            unsafe { PtInRegion(region.0, x, y) },
                            0,
                            "HUD content was clipped"
                        );
                    }
                    assert_eq!(
                        unsafe { GetWindowRgn(untouched.0, region.0) },
                        0,
                        "The main window must remain untouched"
                    );
                }
            }
        }
    }
}
