//! Render the production widgets with synthetic data, without tray, OAuth or processes.
use super::*;

fn fixture() -> App {
    let (tx, rx) = mpsc::channel();
    App {
        native: native_window::MainWindow::default(),
        nav: Nav::Overview,
        st: sys::Collected {
            port: 8080,
            ..Default::default()
        },
        busy: false,
        busy_text: String::new(),
        toast: None,
        tx,
        rx,
        port_str: "8080".into(),
        key_str: "ui-test-secret-only".into(),
        key_visible: false,
        autostart_gw: false,
        autostart_ui: false,
        lan: "192.0.2.10".into(),
        models: Vec::new(),
        model_refresh_id: 0,
        model_loading: false,
        model_error: None,
        gateway_status_error: None,
        free_gateways: [true, true],
        saved_free_gateways: [true, true],
        realm: 0,
        auth_url: String::new(),
        auth_state: None,
        login_profile: None,
        login_target_uid: String::new(),
        login_browser_opened: false,
        oauth_log: Vec::new(),
        acct_busy: false,
        pending_del: None,
        usage: None,
        live: usage_charts::LiveHistory::default(),
        status: None,
        tray: None,
        quit: Arc::new(AtomicBool::new(false)),
        start_hidden: false,
        float_open: Arc::new(AtomicBool::new(false)),
        float_stats: Arc::new(Mutex::new(live_hud::HudStats::default())),
    }
}

fn input(width: f32, height: f32) -> egui::RawInput {
    egui::RawInput {
        screen_rect: Some(egui::Rect::from_min_size(
            egui::Pos2::ZERO,
            egui::vec2(width, height),
        )),
        ..Default::default()
    }
}

fn shape_text(shape: &egui::Shape, text: &mut String) {
    match shape {
        egui::Shape::Text(t) => {
            text.push_str(t.galley.text());
            text.push('\n');
        }
        egui::Shape::Vec(shapes) => {
            for s in shapes {
                shape_text(s, text);
            }
        }
        _ => {}
    }
}

/// Optional software-render evidence: actual egui triangles and font atlas, with fake data only.
fn export_frame(
    ctx: &egui::Context,
    output: egui::FullOutput,
    width: f32,
    height: f32,
    nav: Nav,
    frame: usize,
    variant: &str,
) {
    let Some(dir) = std::env::var_os("WB2API_UI_REVIEW_DIR") else {
        return;
    };
    let dir = std::path::PathBuf::from(dir).join(format!(
        "{}-{}-{}{}",
        width as u32, height as u32, nav as u8, variant
    ));
    fs::create_dir_all(&dir).unwrap();
    let mut textures = Vec::new();
    for (i, (id, delta)) in output.textures_delta.set.iter().enumerate() {
        let pixels: Vec<u8> = match &delta.image {
            egui::ImageData::Color(image) => {
                image.pixels.iter().flat_map(|p| p.to_array()).collect()
            }
            egui::ImageData::Font(image) => image
                .srgba_pixels(None)
                .flat_map(|p| p.to_array())
                .collect(),
        };
        let name = format!("texture-{frame}-{i}.rgba");
        fs::write(dir.join(&name), pixels).unwrap();
        textures.push(serde_json::json!({"id": format!("{id:?}"), "size": delta.image.size(), "pos": delta.pos, "file": name}));
    }
    let primitives = ctx.tessellate(output.shapes, output.pixels_per_point);
    let meshes: Vec<Value> = primitives.into_iter().filter_map(|p| {
        let egui::epaint::Primitive::Mesh(mesh) = p.primitive else { return None; };
        Some(serde_json::json!({
            "clip": [p.clip_rect.min.x, p.clip_rect.min.y, p.clip_rect.max.x, p.clip_rect.max.y],
            "texture": format!("{:?}", mesh.texture_id), "indices": mesh.indices,
            "vertices": mesh.vertices.iter().map(|v| [v.pos.x, v.pos.y, v.uv.x, v.uv.y,
                v.color.r() as f32, v.color.g() as f32, v.color.b() as f32, v.color.a() as f32]).collect::<Vec<_>>()
        }))
    }).collect();
    fs::write(
        dir.join(format!("frame-{frame}.json")),
        serde_json::to_vec(&serde_json::json!({
        "width": width, "height": height, "textures": textures, "meshes": meshes
        }))
        .unwrap(),
    )
    .unwrap();
}

#[test]
fn all_pages_fit_default_and_compact_window() {
    for (width, height) in [(920.0, 580.0), (WIN_W, WIN_H)] {
        for nav in Nav::ALL {
            let ctx = egui::Context::default();
            setup_fonts(&ctx);
            setup_theme(&ctx);
            let mut app = fixture();
            app.nav = nav;
            if nav == Nav::Settings {
                app.float_open.store(true, Ordering::Relaxed);
            }
            let mut content = egui::Vec2::ZERO;
            for frame in 0..3 {
                let output = ctx.run(input(width, height), |ctx| {
                    app.ui_navigation(ctx);
                    content = app.ui_workspace(ctx);
                });
                export_frame(&ctx, output, width, height, nav, frame, "");
            }
            assert!(
                content.x <= width - NAV_W - 56.0 + 1.0,
                "{} at {width}: horizontal overflow, content={content:?}",
                nav.label()
            );
        }
    }
}

#[test]
fn populated_usage_charts_and_model_bars_fit_the_page() {
    for (width, height) in [(920.0, 580.0), (WIN_W, WIN_H)] {
        let ctx = egui::Context::default();
        setup_fonts(&ctx);
        setup_theme(&ctx);
        let mut app = fixture();
        app.nav = Nav::Usage;
        app.live = usage_charts::LiveHistory::review_fixture();
        app.usage = Some(sys::UsageStats {
            today_tokens: 11300,
            total_tokens: 11300,
            today_req: 17,
            total_req: 17,
            ok_req: 17,
            avg_tps: 100.0,
            avg_ttfb_ms: 800.0,
            by_model: vec![
                ("demo/fast".into(), 7000, 10),
                ("demo/deep".into(), 4300, 7),
            ],
            by_uid: vec![("demo-account".into(), 11300, 17)],
            ..Default::default()
        });
        for frame in 0..3 {
            let output = ctx.run(input(width, height), |ctx| {
                app.ui_navigation(ctx);
                let size = app.ui_workspace(ctx);
                assert!(
                    size.x <= width - NAV_W - 56.0 + 1.0,
                    "Chart overflow: {size:?}"
                );
            });
            let mut text = String::new();
            for shape in &output.shapes {
                shape_text(&shape.shape, &mut text);
            }
            assert!(text.contains("实时吞吐"));
            assert!(text.contains("Token 速率"));
            assert!(text.contains("请求速率"));
            export_frame(&ctx, output, width, height, Nav::Usage, frame, "-populated");
        }
    }
}

#[test]
fn long_model_rows_stay_inside_their_scroll_area() {
    let ctx = egui::Context::default();
    setup_fonts(&ctx);
    setup_theme(&ctx);
    let mut app = fixture();
    app.nav = Nav::Models;
    app.models.push(net::ModelRow {
        id: "provider/model-with-an-extremely-long-name-for-layout-check".into(),
        name: "用于布局检查的长模型名称".into(),
        credits: "1.0".into(),
        desc: "这是一段较长的模型说明，用于确认模型表格可以横向滚动而不会挤出页面".into(),
    });
    for (id, credits) in [
        ("free", "x0.00"),
        ("light", "x0.05"),
        ("medium", "x0.65"),
        ("high", "x1.62"),
        ("unknown", "-"),
    ] {
        app.models.push(net::ModelRow {
            id: format!("demo/{id}"),
            name: id.into(),
            credits: credits.into(),
            desc: "倍率配色示例".into(),
        });
    }
    for frame in 0..3 {
        let output = ctx.run(input(920.0, 580.0), |ctx| {
            app.ui_navigation(ctx);
            let content = app.ui_workspace(ctx);
            assert!(
                content.x <= 920.0 - NAV_W - 56.0 + 1.0,
                "model table escaped: {content:?}"
            );
        });
        export_frame(&ctx, output, 920.0, 580.0, Nav::Models, frame, "-rates");
    }
}

#[test]
fn free_gateway_failures_fit_compact_models_page_and_remain_visible() {
    let ctx = egui::Context::default();
    setup_fonts(&ctx);
    setup_theme(&ctx);
    let mut app = fixture();
    app.nav = Nav::Models;
    app.st.running = true;
    app.status = Some(serde_json::json!({"free_gateways": [
        {"id": "opencode", "name": "OpenCode", "enabled": true, "model_count": 4,
            "last_error": "HTTP 403 / FreeTierError: Free tier access unavailable"},
        {"id": "kilo", "name": "Kilo Gateway", "enabled": true, "model_count": 0,
            "last_error": "模型目录请求超时，请稍后重试"}
    ]}));
    for (id, reason) in [("opencode", "FreeTierError"), ("kilo", "请求超时")] {
        let (summary, color) = app.free_gateway_summary(id);
        assert!(summary.contains(reason), "provider failure was hidden: {summary}");
        assert_eq!(color, ERR);
    }
    for frame in 0..3 {
        let output = ctx.run(input(920.0, 580.0), |ctx| {
            app.ui_navigation(ctx);
            let size = app.ui_workspace(ctx);
            assert!(size.x <= 920.0 - NAV_W - 56.0 + 1.0, "gateway card escaped compact page: {size:?}");
        });
        let mut text = String::new();
        for shape in &output.shapes { shape_text(&shape.shape, &mut text); }
        for expected in ["OpenCode", "Kilo Gateway", "opencode:模型ID", "kilo:模型ID", "FreeTierError", "请求超时", "提示词与输出可能"] {
            assert!(text.contains(expected), "compact provider card did not show {expected}: {text}");
        }
        export_frame(&ctx, output, 920.0, 580.0, Nav::Models, frame, "-gateway-errors");
    }
}

#[test]
fn model_directory_success_cannot_mask_provider_failure_or_unknown_status() {
    let ctx = egui::Context::default();
    let mut app = fixture();
    app.st.running = true;
    app.model_refresh_id = 7;
    app.model_loading = true;
    let model = net::ModelRow { id: "opencode:example-free".into(), name: "Example".into(),
        credits: "x0.00".into(), desc: "synthetic model".into() };
    app.handle(Msg::Models(7, Ok(vec![model]), Ok(serde_json::json!({"free_gateways": [
        {"id": "opencode", "enabled": true, "model_count": 1, "last_error": "403 FreeTierError"},
        {"id": "kilo", "enabled": false, "model_count": 0, "last_error": ""}
    ]})), false), &ctx);
    assert_eq!(app.models.len(), 1);
    assert!(!app.model_loading);
    let (toast, ok, _) = app.toast.as_ref().unwrap();
    assert!(!ok);
    assert!(toast.contains("OpenCode 上游异常"));
    app.handle(Msg::Models(6, Ok(Vec::new()), Err("stale".into()), false), &ctx);
    assert_eq!(app.models.len(), 1, "late refresh must not replace a newer result");
    app.handle(Msg::Models(7, Ok(Vec::new()), Err("status timeout".into()), false), &ctx);
    assert!(app.status.is_none());
    assert!(!app.toast.as_ref().unwrap().1, "a model list does not verify gateway health");
    assert!(app.free_gateway_summary("opencode").0.contains("status timeout"));
    app.models.push(net::ModelRow { id: "kilo:cached-free".into(), name: "Cached".into(),
        credits: "x0.00".into(), desc: String::new() });
    app.handle(Msg::Models(7, Err("gateway unavailable".into()), Err("connection refused".into()), false), &ctx);
    assert!(app.models.is_empty(), "failed refresh must not present stale models as current");
    assert_eq!(app.model_error.as_deref(), Some("gateway unavailable"));
    app.model_loading = true;
    assert!(app.free_gateway_summary("opencode").0.contains("正在刷新"));
}

#[test]
fn authorization_link_is_complete_and_wraps_inside_the_login_page() {
    let url = format!("https://copilot.tencent.com/authorize?platform=CLI&state=synthetic-only&redirect_uri=https%3A%2F%2Fexample.invalid%2Freturn%3Fa%3D1%26b%3D2&long={}#callback?last=retained", "a".repeat(2048));
    for (width, height) in [(920.0, 580.0), (WIN_W, WIN_H)] {
        let ctx = egui::Context::default();
        setup_fonts(&ctx);
        setup_theme(&ctx);
        let mut app = fixture();
        app.nav = Nav::AddAcct;
        app.handle(Msg::OAuthUrl(url.clone(), "synthetic-only".into(), None), &ctx);
        for frame in 0..3 {
            let output = ctx.run(input(width, height), |ctx| {
                app.ui_navigation(ctx);
                let size = app.ui_workspace(ctx);
                assert!(size.x <= width - NAV_W - 56.0 + 1.0, "URL escaped login page: {size:?}");
            });
            let mut text = String::new();
            for shape in &output.shapes { shape_text(&shape.shape, &mut text); }
            assert!(text.contains(&url), "login URL was shortened for display");
            assert_eq!(app.auth_url, url, "rendering changed the launch URL");
            export_frame(&ctx, output, width, height, Nav::AddAcct, frame, "-full-url");
        }
    }
}

#[test]
fn key_input_hides_plaintext_until_revealed() {
    let ctx = egui::Context::default();
    setup_theme(&ctx);
    let mut app = fixture();
    for visible in [false, true] {
        app.key_visible = visible;
        let output = ctx.run(input(600.0, 500.0), |ctx| {
            egui::CentralPanel::default().show(ctx, |ui| app.ui_gateway_config(ui));
        });
        let mut text = String::new();
        for shape in output.shapes {
            shape_text(&shape.shape, &mut text);
        }
        assert_eq!(text.contains("ui-test-secret-only"), visible);
    }
}

#[test]
fn settings_switch_is_right_aligned_and_clickable() {
    let ctx = egui::Context::default();
    setup_fonts(&ctx);
    setup_theme(&ctx);
    let mut value = false;
    let mut rect = egui::Rect::NOTHING;
    let _ = ctx.run(input(600.0, 200.0), |ctx| {
        egui::CentralPanel::default().show(ctx, |ui| {
            let right = ui.available_rect_before_wrap().right();
            rect = setting_row(
                ui,
                "Test switch",
                "Synthetic setting, no system changes.",
                &mut value,
            );
            assert!(
                (right - rect.right()).abs() < 1.0,
                "Switch is not aligned with the row edge: {rect:?}"
            );
        });
    });
    for pressed in [true, false] {
        let mut raw = input(600.0, 200.0);
        raw.events = vec![
            egui::Event::PointerMoved(rect.center()),
            egui::Event::PointerButton {
                pos: rect.center(),
                button: egui::PointerButton::Primary,
                pressed,
                modifiers: egui::Modifiers::NONE,
            },
        ];
        let _ = ctx.run(raw, |ctx| {
            egui::CentralPanel::default().show(ctx, |ui| {
                setting_row(
                    ui,
                    "Test switch",
                    "Synthetic setting, no system changes.",
                    &mut value,
                );
            });
        });
    }
    assert!(
        value,
        "Switch did not update its bound setting after a click"
    );
}

#[test]
fn token_hud_fits_its_native_viewport() {
    for (tps, total, variant) in [
        (100.0, 75_478, ""),
        (0.0, 0, "-empty"),
        (12_345.0, u64::MAX, "-large"),
    ] {
        let ctx = egui::Context::default();
        setup_fonts(&ctx);
        setup_theme(&ctx);
        let open = AtomicBool::new(true);
        for frame in 0..3 {
            let output = ctx.run(input(HUD_W, HUD_H), |ctx| {
                egui::CentralPanel::default()
                    .frame(egui::Frame::none())
                    .show(ctx, |ui| {
                        token_hud(ui, &open, &live_hud::HudStats::review_fixture(tps, total));
                        assert!(
                            ui.min_rect().width() <= HUD_W,
                            "HUD horizontal overflow: {:?}",
                            ui.min_rect()
                        );
                        assert!(
                            ui.min_rect().height() <= HUD_H,
                            "HUD vertical overflow: {:?}",
                            ui.min_rect()
                        );
                    });
            });
            for shape in &output.shapes {
                if let egui::Shape::Text(text) = &shape.shape {
                    let bounds = text.galley.rect.translate(text.pos.to_vec2());
                    assert!(
                        bounds.left() >= HUD_GUTTER && bounds.right() <= HUD_W - HUD_GUTTER,
                        "HUD text escaped its surface: {} {bounds:?}",
                        text.galley.text()
                    );
                    if text.galley.text() == "tok/s" {
                        assert!(
                            bounds.right() < 143.0,
                            "Speed overlaps the cumulative total: {bounds:?}"
                        );
                    }
                }
            }
            export_frame(&ctx, output, HUD_W, HUD_H, Nav::Settings, frame, variant);
        }
    }
}

#[test]
fn hud_drag_and_close_have_separate_hit_areas() {
    for close_click in [true, false] {
        let ctx = egui::Context::default();
        setup_fonts(&ctx);
        setup_theme(&ctx);
        let open = AtomicBool::new(true);
        let mut close = egui::Rect::NOTHING;
        let mut commands = Vec::new();
        for step in 0..4 {
            let mut raw = input(HUD_W, HUD_H);
            let click = if close_click {
                close.center()
            } else {
                egui::pos2(80.0, 65.0)
            };
            if step == 1 || step == 3 {
                raw.events = vec![
                    egui::Event::PointerMoved(click),
                    egui::Event::PointerButton {
                        pos: click,
                        button: egui::PointerButton::Primary,
                        pressed: step == 1,
                        modifiers: egui::Modifiers::NONE,
                    },
                ];
            } else if step == 2 && !close_click {
                raw.events = vec![egui::Event::PointerMoved(click + egui::vec2(18.0, 4.0))];
            }
            let output = ctx.run(raw, |ctx| {
                egui::CentralPanel::default()
                    .frame(egui::Frame::none())
                    .show(ctx, |ui| {
                        close = token_hud(ui, &open, &live_hud::HudStats::review_fixture(100.0, 75_478));
                    });
            });
            commands.extend(
                output.viewport_output[&egui::ViewportId::ROOT]
                    .commands
                    .clone(),
            );
        }
        assert_eq!(open.load(Ordering::Relaxed), !close_click);
        assert_eq!(
            commands.iter().any(|c| matches!(c, ViewportCommand::Close)),
            close_click
        );
        assert_eq!(
            commands
                .iter()
                .any(|c| matches!(c, ViewportCommand::StartDrag)),
            !close_click
        );
    }
}

#[test]
fn controls_remain_dark_when_windows_reports_light_theme() {
    let ctx = egui::Context::default();
    setup_theme(&ctx);
    for system_theme in [egui::Theme::Light, egui::Theme::Dark, egui::Theme::Light] {
        let mut raw = input(600.0, 300.0);
        raw.system_theme = Some(system_theme);
        let _ = ctx.run(raw, |ctx| {
            assert!(
                ctx.style().visuals.dark_mode,
                "Windows theme changed native controls to light"
            );
            assert_eq!(ctx.style().visuals.override_text_color, Some(TX));
            assert_eq!(ctx.style().visuals.extreme_bg_color, BG);
        });
    }
}

#[test]
fn action_buttons_keep_their_visible_hit_area_and_disabled_state() {
    for (click, enabled, expected) in [
        (egui::pos2(45.0, 25.0), true, true),
        (egui::pos2(550.0, 25.0), true, false),
        (egui::pos2(45.0, 25.0), false, false),
    ] {
        let ctx = egui::Context::default();
        setup_theme(&ctx);
        let mut clicked = false;
        for pressed in [None, Some(true), Some(false)] {
            let mut raw = input(600.0, 200.0);
            if let Some(pressed) = pressed {
                raw.events = vec![
                    egui::Event::PointerMoved(click),
                    egui::Event::PointerButton {
                        pos: click,
                        button: egui::PointerButton::Primary,
                        pressed,
                        modifiers: egui::Modifiers::NONE,
                    },
                ];
            }
            let _ = ctx.run(raw, |ctx| {
                egui::CentralPanel::default().show(ctx, |ui| {
                    ui.with_layout(egui::Layout::top_down_justified(egui::Align::Min), |ui| {
                        clicked |= btn(ui, "Start", ACC, INK, enabled);
                    });
                });
            });
        }
        assert_eq!(
            clicked, expected,
            "Button click {click:?}, enabled {enabled}"
        );
    }
}

#[test]
fn tray_window_and_hud_actions_still_work_while_gateway_is_busy() {
    let ctx = egui::Context::default();
    let mut app = fixture();
    app.busy = true;
    let output = ctx.run(input(WIN_W, WIN_H), |ctx| {
        app.handle(Msg::Tray(TrayAct::ToggleHud), ctx);
        app.handle(Msg::Tray(TrayAct::Show), ctx);
    });
    assert!(app.float_open.load(Ordering::Relaxed));
    let commands = &output.viewport_output[&egui::ViewportId::ROOT].commands;
    assert!(
        commands
            .iter()
            .any(|c| matches!(c, ViewportCommand::Visible(true)))
    );
    assert!(commands.iter().any(|c| matches!(c, ViewportCommand::Focus)));
    let output = ctx.run(input(WIN_W, WIN_H), |ctx| {
        app.handle(Msg::Tray(TrayAct::ToggleHud), ctx);
        app.handle(Msg::Tray(TrayAct::Quit), ctx);
    });
    assert!(!app.float_open.load(Ordering::Relaxed));
    assert!(app.quit.load(Ordering::SeqCst));
    assert!(
        output.viewport_output[&egui::ViewportId::ROOT]
            .commands
            .iter()
            .any(|c| matches!(c, ViewportCommand::Close))
    );
}
