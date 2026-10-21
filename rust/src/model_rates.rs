//! Stable rate colors: free is green, higher paid multipliers deepen the translucent red.
use eframe::egui::{self, Color32, RichText};

pub struct RateStyle {
    pub fill: Color32,
    foreground: Color32,
}

fn multiplier(credits: &str) -> Option<f64> {
    let credits = credits.trim();
    if matches!(credits, "免费" | "限时免费") || credits.eq_ignore_ascii_case("free") {
        return Some(0.0);
    }
    let value = credits
        .trim_start_matches(['x', 'X', '×'])
        .trim_end_matches(['x', 'X', '×', '倍'])
        .trim()
        .parse::<f64>()
        .ok()?;
    (value.is_finite() && value >= 0.0).then_some(value)
}

pub fn style(credits: &str) -> RateStyle {
    match multiplier(credits) {
        Some(0.0) => RateStyle {
            fill: Color32::from_rgba_unmultiplied(74, 190, 128, 36),
            foreground: super::OK,
        },
        Some(rate) => {
            // Fixed scale keeps the same rate the same color after a refresh.
            // A smooth curve distinguishes fractional rates and leaves room for higher rates.
            let depth = (rate / (rate + 0.65)) as f32;
            let mix = |low: f32, high: f32| (low + (high - low) * depth).round() as u8;
            RateStyle {
                fill: Color32::from_rgba_unmultiplied(
                    mix(230.0, 244.0),
                    mix(137.0, 63.0),
                    mix(126.0, 80.0),
                    mix(28.0, 92.0),
                ),
                foreground: Color32::from_rgb(
                    mix(234.0, 255.0),
                    mix(177.0, 112.0),
                    mix(170.0, 123.0),
                ),
            }
        }
        None => RateStyle {
            fill: Color32::from_rgba_unmultiplied(139, 142, 156, 16),
            foreground: super::TX3,
        },
    }
}

pub fn badge(ui: &mut egui::Ui, credits: &str) {
    let colors = style(credits);
    let hint = match multiplier(credits) {
        Some(0.0) => "免费模型",
        Some(_) => "积分倍率越高，红色越深",
        None => "上游未提供有效倍率",
    };
    egui::Frame::none()
        .fill(colors.fill)
        .rounding(egui::Rounding::same(5.0))
        .inner_margin(egui::Margin::symmetric(7.0, 3.0))
        .show(ui, |ui| {
            ui.label(RichText::new(credits).color(colors.foreground).size(12.0));
        })
        .response
        .on_hover_text(hint);
}
