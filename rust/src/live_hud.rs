//! Once-per-second display snapshots. In-flight estimates never enter saved usage totals.
use crate::sys::UsageStats;
use eframe::egui::{self, Color32};
use serde::Deserialize;
use std::collections::VecDeque;

#[derive(Clone, Default, Deserialize)]
pub struct LiveUsage {
    pub instance: String,
    pub active_requests: u64,
    pub completed_tokens: u64,
    pub active_tokens_estimated: u64,
    pub observed_tokens_estimated: f64,
}

#[derive(Clone, Default)]
pub struct HudStats {
    pub tps: f32,
    pub total: u64,
    pub estimated: bool,
    pub connected: bool,
    history: VecDeque<(f64, f32)>,
    previous: Option<(f64, String, f64)>,
}

impl HudStats {
    pub fn sample(&mut self, now: f64, usage: &UsageStats, live: Option<&LiveUsage>) {
        self.tps = 0.0;
        self.total = usage.total_tokens;
        self.estimated = false;
        self.connected = live.is_some();
        if let Some(live) = live {
            // The matching startup marker separates persisted history from this process.
            // Completed requests move from the estimate into the exact counter only once.
            if usage.live_instance == live.instance {
                self.total = usage
                    .live_baseline_tokens
                    .saturating_add(live.completed_tokens);
            }
            self.total = self.total.saturating_add(live.active_tokens_estimated);
            self.estimated = live.active_requests > 0;
            let observed = live.observed_tokens_estimated;
            if let Some((previous, instance, old)) = &self.previous {
                let elapsed = now - previous;
                if instance == &live.instance && elapsed >= 0.5 && observed.is_finite() {
                    // An idle or stalled stream is zero, regardless of historic averages.
                    if live.active_requests > 0 && observed >= *old {
                        self.tps = ((observed - old) / elapsed).max(0.0) as f32;
                    }
                }
            }
            self.previous = Some((now, live.instance.clone(), observed));
        } else {
            // A stopped/old/unreachable gateway must never leave a stale speed on screen.
            self.previous = None;
        }
        if !self.tps.is_finite() {
            self.tps = 0.0;
        }
        self.history.push_back((now, self.tps));
        while self.history.len() > 1 && self.history[0].0 < now - 60.0 {
            self.history.pop_front();
        }
        // Bound memory even if a caller samples more often than once a second.
        while self.history.len() > 121 {
            self.history.pop_front();
        }
    }

    pub fn paint_background(&self, painter: &egui::Painter, surface: egui::Rect) {
        let plot = egui::Rect::from_min_max(
            surface.min + egui::vec2(8.0, 29.0),
            surface.max - egui::vec2(8.0, 4.0),
        );
        let p = painter.with_clip_rect(plot);
        let now = self.history.back().map(|s| s.0).unwrap_or(0.0);
        let max = self.history.iter().map(|s| s.1).fold(10.0_f32, f32::max) * 1.2;
        let points: Vec<_> = self
            .history
            .iter()
            .map(|(time, value)| {
                egui::pos2(
                    plot.right() - ((now - time) / 60.0) as f32 * plot.width(),
                    plot.bottom() - value / max * plot.height(),
                )
            })
            .collect();
        p.line_segment(
            [plot.left_bottom(), plot.right_bottom()],
            egui::Stroke::new(0.6, crate::ACC.gamma_multiply(0.18)),
        );
        if points.len() < 2 {
            return;
        }
        let mut area = egui::epaint::Mesh::default();
        for pair in points.windows(2) {
            let base = area.vertices.len() as u32;
            for (pos, color) in [
                (pair[0], crate::ACC.gamma_multiply(0.14)),
                (pair[1], crate::ACC.gamma_multiply(0.14)),
                (egui::pos2(pair[0].x, plot.bottom()), Color32::TRANSPARENT),
                (egui::pos2(pair[1].x, plot.bottom()), Color32::TRANSPARENT),
            ] {
                area.colored_vertex(pos, color);
            }
            area.add_triangle(base, base + 1, base + 2);
            area.add_triangle(base + 1, base + 3, base + 2);
        }
        p.add(egui::Shape::mesh(area));
        p.add(egui::Shape::line(
            points,
            egui::Stroke::new(1.3, crate::ACC.gamma_multiply(0.42)),
        ));
    }

    #[cfg(test)]
    pub fn review_fixture(tps: f32, total: u64) -> Self {
        let mut stats = Self {
            tps,
            total,
            estimated: tps > 0.0,
            connected: true,
            ..Self::default()
        };
        for second in 0..=60 {
            let value = if tps == 0.0 {
                0.0
            } else {
                tps * (0.6
                    + 0.2 * (second as f32 * 0.7).sin()
                    + 0.15 * (second as f32 * 0.23).cos())
            };
            stats.history.push_back((second as f64, value));
        }
        stats
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn live_totals_tick_without_a_completed_request_and_settle_without_double_counting() {
        let usage = UsageStats {
            total_tokens: 1_000,
            live_instance: "one".into(),
            live_baseline_tokens: 1_000,
            ..Default::default()
        };
        let mut live = LiveUsage {
            instance: "one".into(),
            active_requests: 1,
            ..Default::default()
        };
        let mut hud = HudStats::default();
        hud.sample(0.0, &usage, Some(&live));
        live.active_tokens_estimated = 50;
        live.observed_tokens_estimated = 50.0;
        hud.sample(1.0, &usage, Some(&live));
        assert_eq!((hud.tps, hud.total), (50.0, 1_050));
        live.active_tokens_estimated = 130;
        live.observed_tokens_estimated = 130.0;
        hud.sample(2.0, &usage, Some(&live));
        assert_eq!((hud.tps, hud.total), (80.0, 1_130));
        live.active_requests = 0;
        live.active_tokens_estimated = 0;
        live.completed_tokens = 121;
        hud.sample(3.0, &usage, Some(&live));
        assert_eq!((hud.tps, hud.total, hud.estimated), (0.0, 1_121, false));
        // The same completion appearing in the log cannot be counted a second time.
        let logged = UsageStats {
            total_tokens: 1_121,
            ..usage
        };
        hud.sample(4.0, &logged, Some(&live));
        assert_eq!(hud.total, 1_121);
    }

    #[test]
    fn idle_stalled_disconnected_and_restarted_gateways_have_no_stale_speed() {
        let usage = UsageStats {
            total_tokens: 75_000,
            avg_tps: 92.0,
            ..Default::default()
        };
        let mut live = LiveUsage {
            instance: "one".into(),
            active_requests: 2,
            observed_tokens_estimated: 100.0,
            ..Default::default()
        };
        let mut hud = HudStats::default();
        hud.sample(0.0, &usage, Some(&live));
        assert_eq!(hud.tps, 0.0);
        live.observed_tokens_estimated = 500.0;
        hud.sample(2.0, &usage, Some(&live));
        assert_eq!(hud.tps, 200.0);
        hud.sample(3.0, &usage, Some(&live));
        assert_eq!(
            hud.tps, 0.0,
            "No new stream data means zero even with requests in flight"
        );
        live.instance = "two".into();
        live.observed_tokens_estimated = 10_000.0;
        hud.sample(4.0, &usage, Some(&live));
        assert_eq!(hud.tps, 0.0, "A process restart must not create a spike");
        hud.sample(5.0, &usage, None);
        assert_eq!((hud.tps, hud.total, hud.connected), (0.0, 75_000, false));
        for second in 6..600 {
            hud.sample(second as f64, &usage, None);
        }
        assert!(hud.history.len() <= 61);
    }
}
