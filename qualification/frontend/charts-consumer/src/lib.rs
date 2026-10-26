//! Qualification-only external consumer. All observations are synthetic;
//! this crate does not connect to Homeward or contain installation configuration.
#![deny(clippy::wildcard_enum_match_arm)]
slint::include_modules!();

#[cfg(test)]
mod tests {
    use super::*;
    use slint::{
        ComponentHandle, ModelRc, VecModel,
        platform::{Key, WindowEvent},
    };
    use slint_kit_pro::charts::*;
    use std::{
        rc::Rc,
        sync::{Arc, mpsc},
        time::Duration,
    };

    #[derive(Clone, Copy)]
    enum Domain {
        MachineTelemetry,
        ProcessingDelays,
    }
    impl Domain {
        fn heading(self) -> &'static str {
            match self {
                Self::MachineTelemetry => "Synthetic machine telemetry",
                Self::ProcessingDelays => "Synthetic processing delays",
            }
        }
        fn unit(self) -> &'static str {
            match self {
                Self::MachineTelemetry => "%",
                Self::ProcessingDelays => "ms",
            }
        }
        fn kind(self) -> PlotKind {
            match self {
                Self::MachineTelemetry => PlotKind::Curve,
                Self::ProcessingDelays => PlotKind::Bars {
                    baseline: Baseline::new(0.).unwrap(),
                    width: BarWidth::new(2.).unwrap(),
                },
            }
        }
    }
    fn time(ms: i64) -> Timestamp {
        Timestamp::from_millis(ms)
    }
    fn span(start: i64, end: i64) -> TimeSpan {
        TimeSpan::new(time(start), time(end)).unwrap()
    }
    fn source() -> Arc<SeriesSet> {
        Arc::new(
            SeriesSet::new(
                (0..2)
                    .map(|index| {
                        let samples = (0..20_000)
                            .map(|i| {
                                Sample::new(
                                    time(
                                        i * 1000
                                            + (i % 9) * 13
                                            + if i >= 10_000 { 1_800_000 } else { 0 },
                                    ),
                                    ((i * 13 + index * 173) % 800) as f64 / 8.,
                                )
                                .unwrap()
                            })
                            .collect();
                        Arc::new(
                            Series::new(
                                SeriesId::new(format!("synthetic-{index}")).unwrap(),
                                samples,
                                vec![span(10_000_000, 11_800_000)],
                            )
                            .unwrap(),
                        )
                    })
                    .collect(),
            )
            .unwrap(),
        )
    }
    #[test]
    fn external_public_consumer_uses_two_domains_and_custom_skin_at_double_text() {
        i_slint_backend_testing::init_no_event_loop();
        for domain in [Domain::MachineTelemetry, Domain::ProcessingDelays] {
            let data = source();
            let (tx, rx) = mpsc::channel();
            let mut worker = ChartWorker::new(move |notice| {
                let _ = tx.send(notice);
            })
            .unwrap();
            let colors = [[127, 33, 201], [32, 155, 99]];
            let style = RasterStyle::new(
                [240, 235, 245],
                data.series()
                    .iter()
                    .zip(colors)
                    .map(|(series, rgb)| {
                        SeriesPaint::new(series.id().clone(), rgb, 255, 2).unwrap()
                    })
                    .collect(),
            )
            .unwrap();
            let id = worker
                .submit(RenderRequest {
                    data: data.clone(),
                    viewport: Viewport {
                        interval: span(0, 21_801_000),
                        width: PixelWidth::new(800).unwrap(),
                    },
                    height: PixelHeight::new(240).unwrap(),
                    values: ValueRange::new(0., 100.).unwrap(),
                    kind: domain.kind(),
                    style,
                })
                .unwrap();
            let notice = rx.recv_timeout(Duration::from_secs(5)).unwrap();
            let Some(PreparationResult::Ready(frame)) = notice.take_ready() else {
                panic!("external worker frame missing");
            };
            assert_eq!(frame.id(), id);
            assert_eq!(frame.stats().source_samples, 40_000);
            assert!(frame.stats().projected_points <= 2 * (4 * 800 + 2));
            assert_ne!(frame.stats().worker_thread, std::thread::current().id());
            assert!(matches!(
                data.series()[0].inspect_nearest(time(10_900_500)),
                Inspection::Gap(_)
            ));
            let window = ExternalCharts::new().unwrap();
            let mut theme = window.global::<KitThemes>().get_light();
            theme.text_scale = 2.;
            theme.accent = slint::Color::from_rgb_u8(127, 33, 201);
            window.set_theme(theme);
            window.set_heading(domain.heading().into());
            window.set_unit(domain.unit().into());
            window.set_generation(id.generation());
            let bitmap = frame.bitmap();
            window.set_source_width(i32::from(bitmap.width()));
            window.set_source_height(i32::from(bitmap.height()));
            window.set_pixels(slint::Image::from_rgb8(slint::SharedPixelBuffer::<
                slint::Rgb8Pixel,
            >::clone_from_slice(
                bitmap.pixels(),
                u32::from(bitmap.width()),
                u32::from(bitmap.height()),
            )));
            window.set_series(ModelRc::from(Rc::new(VecModel::from(
                frame
                    .series()
                    .iter()
                    .enumerate()
                    .map(|(index, series)| ProChartSeries {
                        token: index as i32 + 1,
                        label: series.id().as_str().into(),
                        active: index == 0,
                        color: slint::Color::from_rgb_u8(
                            colors[index][0],
                            colors[index][1],
                            colors[index][2],
                        ),
                        mixed_gap_commands: series.geometry().mixed_gap_commands.clone().into(),
                        missing_gap_commands: series.geometry().missing_gap_commands.clone().into(),
                    })
                    .collect::<Vec<_>>(),
            ))));
            let current = std::cell::Cell::new(0usize);
            let weak = window.as_weak();
            let original = data.series()[0].clone();
            window.on_navigate(move |generation, step, _extend| {
                assert_eq!(generation, id.generation());
                let samples = original.samples();
                let index = match step {
                    NavigationStep::First => 0,
                    NavigationStep::Last => samples.len() - 1,
                    NavigationStep::Next => (current.get() + 1).min(samples.len() - 1),
                    NavigationStep::Previous => current.get().saturating_sub(1),
                    NavigationStep::PageNext => (current.get() + 100).min(samples.len() - 1),
                    NavigationStep::PagePrevious => current.get().saturating_sub(100),
                };
                current.set(index);
                let sample = samples[index];
                weak.upgrade().unwrap().set_inspection(
                    format!(
                        "Original {} ms: {} {}",
                        sample.time().millis(),
                        sample.value(),
                        domain.unit()
                    )
                    .into(),
                );
            });
            window.show().unwrap();
            window.invoke_focus_plot();
            for (key, expected) in [
                (Key::Home, format!("Original 0 ms: 0 {}", domain.unit())),
                (
                    Key::End,
                    format!("Original 21799013 ms: 98.375 {}", domain.unit()),
                ),
            ] {
                window
                    .window()
                    .dispatch_event(WindowEvent::KeyPressed { text: key.into() });
                window
                    .window()
                    .dispatch_event(WindowEvent::KeyReleased { text: key.into() });
                assert_eq!(window.get_inspection().as_str(), expected);
            }
            window.hide().unwrap();
            worker.stop();
        }
    }
}
