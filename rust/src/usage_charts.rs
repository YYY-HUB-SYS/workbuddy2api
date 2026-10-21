//! Native charts driven by deltas of completed requests in the gateway log.
use super::{ACC, ACC2, CARD2, TX, TX2, TX3, card, fmt_num, sys};
use eframe::egui::{self, Color32, FontId, RichText};
use std::collections::VecDeque;
use std::time::Instant;

const WINDOW: f64 = 60.0;

#[derive(Clone, Copy)]
struct Sample {
    at: f64,
    tokens: f64,
    requests: f64,
}

pub struct LiveHistory {
    started: Instant,
    baseline: Option<(f64, u64, u64)>,
    samples: VecDeque<Sample>,
}

impl Default for LiveHistory {
    fn default() -> Self {
        Self {
            started: Instant::now(),
            baseline: None,
            samples: VecDeque::new(),
        }
    }
}

impl LiveHistory {
    #[cfg(test)]
    pub fn review_fixture() -> Self {
        let mut history = Self {
            started: Instant::now() - std::time::Duration::from_secs(60),
            ..Self::default()
        };
        let (mut tokens, mut requests) = (5000, 5);
        history.record_at(0.0, tokens, requests);
        for second in 1..=60 {
            if second % 5 == 0 {
                tokens += 200 + second * 10;
                requests += 1;
            }
            history.record_at(second as f64, tokens, requests);
        }
        history
    }

    pub fn record(&mut self, usage: &sys::UsageStats) {
        self.record_at(
            self.started.elapsed().as_secs_f64(),
            usage.total_tokens,
            usage.total_req,
        );
    }

    fn record_at(&mut self, now: f64, tokens: u64, requests: u64) {
        let Some((previous, old_tokens, old_requests)) = self.baseline else {
            self.baseline = Some((now, tokens, requests));
            self.samples.push_back(Sample {
                at: now,
                tokens: 0.0,
                requests: 0.0,
            });
            return;
        };
        if now < previous {
            return;
        }
        if tokens < old_tokens || requests < old_requests {
            self.samples.clear();
            self.baseline = Some((now, tokens, requests));
            self.samples.push_back(Sample {
                at: now,
                tokens: 0.0,
                requests: 0.0,
            });
            return;
        }
        // Ignore duplicate manual refreshes; rates always use the actual sample interval.
        let seconds = now - previous;
        if seconds < 0.5 {
            return;
        }
        self.samples.push_back(Sample {
            at: now,
            tokens: (tokens - old_tokens) as f64 / seconds,
            requests: (requests - old_requests) as f64 * 60.0 / seconds,
        });
        self.baseline = Some((now, tokens, requests));
        while self.samples.len() > 2 && self.samples[1].at < now - WINDOW {
            self.samples.pop_front();
        }
    }

    pub fn show(&self, ui: &mut egui::Ui) {
        card(ui, "实时吞吐", |ui| {
            ui.columns(2, |cols| {
                self.chart(&mut cols[0], "Token 速率", "tok/s", ACC, false);
                self.chart(&mut cols[1], "请求速率", "次/分", ACC2, true);
            });
            ui.add_space(6.0);
            ui.label(
                RichText::new("最近 60 秒 · 每秒采样 · 根据已完成请求的日志增量计算")
                    .color(TX3)
                    .size(11.0),
            );
        });
    }

    fn chart(&self, ui: &mut egui::Ui, title: &str, unit: &str, color: Color32, requests: bool) {
        let value = |sample: &Sample| {
            if requests {
                sample.requests
            } else {
                sample.tokens
            }
        };
        let latest = self.samples.back().map(value).unwrap_or(0.0);
        ui.horizontal(|ui| {
            ui.label(RichText::new(title).color(TX2).size(12.0));
            ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                ui.label(
                    RichText::new(format!("{:.1} {}", latest, unit))
                        .color(color)
                        .size(13.0),
                );
            });
        });
        let (bounds, response) = ui.allocate_exact_size(
            egui::vec2(ui.available_width(), 128.0),
            egui::Sense::hover(),
        );
        let plot = egui::Rect::from_min_max(
            bounds.min + egui::vec2(34.0, 10.0),
            bounds.max - egui::vec2(8.0, 22.0),
        );
        let painter = ui.painter_at(bounds);
        let now = self
            .started
            .elapsed()
            .as_secs_f64()
            .max(self.samples.back().map(|s| s.at).unwrap_or(0.0));
        let maximum = self
            .samples
            .iter()
            .filter(|s| s.at >= now - WINDOW)
            .map(value)
            .fold(0.0_f64, f64::max);
        let ceiling = nice_ceiling(maximum * 1.15);
        let guide = egui::Stroke::new(0.7, Color32::from_gray(47));
        for fraction in [0.0_f32, 0.5, 1.0] {
            let y = plot.bottom() - fraction * plot.height();
            painter.line_segment(
                [egui::pos2(plot.left(), y), egui::pos2(plot.right(), y)],
                guide,
            );
        }
        for (point, align, label) in [
            (
                plot.left_top() - egui::vec2(5.0, 0.0),
                egui::Align2::RIGHT_TOP,
                compact(ceiling),
            ),
            (
                plot.left_bottom() - egui::vec2(5.0, 0.0),
                egui::Align2::RIGHT_BOTTOM,
                "0".into(),
            ),
            (
                plot.left_bottom() + egui::vec2(0.0, 7.0),
                egui::Align2::LEFT_TOP,
                "60 秒前".into(),
            ),
            (
                plot.right_bottom() + egui::vec2(0.0, 7.0),
                egui::Align2::RIGHT_TOP,
                "现在".into(),
            ),
        ] {
            painter.text(point, align, label, FontId::proportional(10.0), TX3);
        }
        let position = |s: &Sample| {
            egui::pos2(
                plot.right() - ((now - s.at) / WINDOW) as f32 * plot.width(),
                plot.bottom() - (value(s) / ceiling) as f32 * plot.height(),
            )
        };
        let points: Vec<_> = self.samples.iter().map(position).collect();
        let graph = painter.with_clip_rect(plot);
        if points.len() >= 2 {
            let mut area = egui::epaint::Mesh::default();
            for pair in points.windows(2) {
                let base = area.vertices.len() as u32;
                for (pos, color) in [
                    (pair[0], color.gamma_multiply(0.14)),
                    (pair[1], color.gamma_multiply(0.14)),
                    (egui::pos2(pair[0].x, plot.bottom()), Color32::TRANSPARENT),
                    (egui::pos2(pair[1].x, plot.bottom()), Color32::TRANSPARENT),
                ] {
                    area.colored_vertex(pos, color);
                }
                area.add_triangle(base, base + 1, base + 2);
                area.add_triangle(base + 1, base + 3, base + 2);
            }
            graph.add(egui::Shape::mesh(area));
            graph.add(egui::Shape::line(points, egui::Stroke::new(1.8, color)));
        } else {
            painter.text(
                plot.center(),
                egui::Align2::CENTER_CENTER,
                "等待采样",
                FontId::proportional(11.0),
                TX3,
            );
        }
        if let Some(pointer) = response.hover_pos().filter(|p| plot.contains(*p)) {
            let sample = self
                .samples
                .iter()
                .filter(|s| s.at >= now - WINDOW)
                .min_by(|a, b| {
                    (position(a).x - pointer.x)
                        .abs()
                        .total_cmp(&(position(b).x - pointer.x).abs())
                });
            if let Some(sample) = sample {
                let point = position(sample);
                graph.line_segment(
                    [
                        egui::pos2(point.x, plot.top()),
                        egui::pos2(point.x, plot.bottom()),
                    ],
                    egui::Stroke::new(0.8, TX3),
                );
                graph.circle_filled(point, 3.0, color);
                response.on_hover_ui(|ui| {
                    ui.label(format!("{:.0} 秒前", (now - sample.at).max(0.0)));
                    ui.label(
                        RichText::new(format!("{}  {:.1} {}", title, value(sample), unit))
                            .color(color),
                    );
                });
            }
        }
    }
}

fn nice_ceiling(value: f64) -> f64 {
    if value <= 1.0 {
        return 1.0;
    }
    let power = 10.0_f64.powf(value.log10().floor());
    [1.0, 2.0, 5.0, 10.0]
        .into_iter()
        .find(|step| step * power >= value)
        .unwrap_or(10.0)
        * power
}

fn compact(value: f64) -> String {
    if value >= 1_000_000.0 {
        format!("{:.1}m", value / 1_000_000.0)
    } else if value >= 1000.0 {
        format!("{:.1}k", value / 1000.0)
    } else {
        format!("{:.0}", value)
    }
}

pub fn model_share(ui: &mut egui::Ui, tokens: u64, total: u64) {
    let fraction = if total == 0 {
        0.0
    } else {
        (tokens as f64 / total as f64).clamp(0.0, 1.0) as f32
    };
    let (rect, response) = ui.allocate_exact_size(egui::vec2(132.0, 20.0), egui::Sense::hover());
    let track = egui::Rect::from_min_size(rect.min + egui::vec2(0.0, 7.0), egui::vec2(82.0, 6.0));
    ui.painter().rect_filled(track, 3.0, CARD2);
    if fraction > 0.0 {
        ui.painter().rect_filled(
            egui::Rect::from_min_size(
                track.min,
                egui::vec2(track.width() * fraction, track.height()),
            ),
            3.0,
            ACC,
        );
    }
    ui.painter().text(
        rect.right_center(),
        egui::Align2::RIGHT_CENTER,
        format!("{:.1}%", fraction * 100.0),
        FontId::proportional(11.0),
        TX,
    );
    response.on_hover_text(format!("{} / {} tokens", fmt_num(tokens), fmt_num(total)));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn live_rates_ignore_old_totals_and_follow_real_elapsed_time() {
        let mut history = LiveHistory::default();
        history.record_at(0.0, 75_000, 56);
        assert_eq!(history.samples.back().unwrap().tokens, 0.0);
        history.record_at(2.0, 75_400, 58);
        assert_eq!(history.samples.back().unwrap().tokens, 200.0);
        assert_eq!(history.samples.back().unwrap().requests, 60.0);
        history.record_at(2.1, 75_400, 58);
        assert_eq!(
            history.samples.len(),
            2,
            "Manual refresh must not generate duplicate spikes"
        );
        history.record_at(3.0, 75_400, 58);
        assert_eq!(history.samples.back().unwrap().tokens, 0.0);
        assert_eq!(history.samples.back().unwrap().requests, 0.0);
    }

    #[test]
    fn live_window_is_bounded_and_log_reset_does_not_create_spikes() {
        let mut history = LiveHistory::default();
        for time in 0..600 {
            history.record_at(time as f64, time * 100, time);
        }
        assert!(history.samples.len() <= 62);
        history.record_at(600.0, 20, 1);
        assert_eq!(history.samples.len(), 1);
        assert_eq!(history.samples[0].tokens, 0.0);
        history.record_at(601.0, 40, 2);
        assert_eq!(history.samples.back().unwrap().tokens, 20.0);
    }
}
