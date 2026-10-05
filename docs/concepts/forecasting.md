# Forecasting

Hybernate uses a **Holt-Winters double seasonal model** to learn your workload's demand patterns and predict future resource needs. This page covers the mathematical model, how it builds confidence over time, and how it handles pattern shifts.

## The Idea

Most workloads follow predictable patterns:

- **Daily**: Traffic peaks during business hours, drops overnight
- **Weekly**: Weekdays are busier than weekends

Hybernate learns these two patterns independently by feeding hourly CPU observations into a Holt-Winters model. Once it has enough data and confidence, it uses the learned patterns to:

- Hold off a pause when it's confident demand is coming in the next hour. It never causes a pause: the [activity clock](idle-detection.md) decides that
- Wake paused workloads ahead of predicted demand, starting 15 minutes before an hour it expects to be busy

## How It Works

The model tracks four components:

| Component | What it represents | Smoothing parameter |
|-----------|-------------------|-------------------|
| **Level** | The mean demand | \( \alpha = 0.01 \) |
| **Trend** | Whether demand is growing or shrinking | \( \beta = 0.001 \) |
| **Daily components** | 24 offsets, one per hour of day: the shape of an average day | \( \gamma_1 = 0.1 \) |
| **Weekly components** | 168 offsets, one per hour of week: how each weekday departs from that shape | \( \gamma_2 = 0.5 \) |

Each hour, the model records the actual CPU observation \( Y(t) \), compares it to what it predicted (for confidence scoring), and updates all four components using exponential smoothing. The model is **additive**: idle workloads spend much of their time at zero demand, where a multiplicative model, which divides by the level and the seasonal factors, diverges.

\[
\begin{aligned}
L(t) &= \max\Bigl(0,\; \alpha \cdot \bigl(Y(t) - D(t) - W(t)\bigr) + (1 - \alpha) \cdot \bigl(L(t{-}k) + k \cdot T(t{-}k)\bigr)\Bigr) \\[6pt]
T(t) &= \beta \cdot \frac{L(t) - L(t{-}k)}{k} + (1 - \beta) \cdot T(t{-}k) \\[6pt]
D(t) &= \gamma_1 \cdot \bigl(Y(t) - L(t) - W(t)\bigr) + (1 - \gamma_1) \cdot D(t - s_1) \\[6pt]
W(t) &= \gamma_2 \cdot \bigl(Y(t) - L(t) - D(t)\bigr) + (1 - \gamma_2) \cdot W(t - s_2)
\end{aligned}
\]

Where \( s_1 = 24 \) (daily season length), \( s_2 = 168 \) (weekly season length), and \( k \) is the number of hours since the previous observation (1, unless observations were missed).

After each update the components are renormalised: the daily components sum to zero, and for each hour of the day the weekly components across the seven days sum to zero. The shift is moved into the component above, which leaves every forecast unchanged and keeps the level the mean demand, so it is never negative.

The forecast for \( h \) hours ahead is never negative either:

\[
F(t+h) = \max\Bigl(0,\; L(t) + h \cdot T(t) + D(t+h) + W(t+h)\Bigr)
\]

A gap in the observations, from the operator being down or a workload paused without the doorman, is skipped rather than filled in: there is no honest value to fill it with, and since slots are wall-clock hours (below), skipping an hour doesn't shift what was learned. The trend is carried across a gap of at most a day.

## Phase Lifecycle

The engine doesn't start making decisions immediately. It progresses through phases as it collects data and builds confidence:

`Observing` → `DailySuggesting` → `DailyActive` → `WeeklySuggesting` → `FullyActive`

| Phase | Requires | Behavior |
|-------|----------|----------|
| **Observing** | Not every hour of the day observed yet | Collecting data. No predictions yet. |
| **DailySuggesting** | Every hour of the day observed at least once | Daily confidence is being scored; predictions aren't acted on yet. |
| **DailyActive** | Daily confidence >= threshold | Predictions drive decisions. Weekly patterns still learning. |
| **WeeklySuggesting** | Every hour of the week observed at least once | Weekly confidence still being earned. |
| **FullyActive** | Weekly confidence >= threshold | Both daily and weekly patterns drive decisions. |

The gates count hours of the calendar, not data points: a workload that is only ever observed from 9 to 5 hasn't shown what happens at night, however many hours it has been watched.

That has a consequence for workloads paused without the [doorman](wake-on-request.md): an hour paused without it isn't observed (see [Wall-Clock Alignment](#wall-clock-alignment)), so a workload paused every night that way never has its nights observed, and its forecast never reaches `DailySuggesting`. Keep wake on request on if you want `autoResume` and the forecast's veto.

The ManagedWorkload shows the phase per season, in `status.prediction`: `dailyPhase` and `weeklyPhase` are each `Observing`, `Suggesting` or `Active`, next to `dailyConfidence` and `weeklyConfidence`. `DailyActive`, for example, is `dailyPhase: Active` with `weeklyPhase: Observing`.

The confidence threshold is configurable per workload via `spec.prediction.confidence` (default 85%, minimum 50%), and a change applies at the next reconcile. A phase is earned at the threshold and lost 5 points below it, so confidence hovering at the threshold doesn't switch the forecast on and off every hour.

## Confidence Scoring

Confidence is \( 1 - \text{WAPE} \), the **weighted absolute percentage error**: the total absolute error of the hourly forecasts over a window, divided by the total demand in it.

\[
C = 1 - \frac{\sum_{i} |F(i) - Y(i)|}{\max\bigl(\sum_{i} Y(i),\; n \cdot f\bigr)}
\]

A confidence of 85% means the forecast's total error over the window is 15% of the demand in it. Unlike an average of per-hour percentage errors, it is defined when demand is zero, and an hour of zero demand forecast as zero isn't counted as a perfect hour. The denominator is never less than \( n \cdot f \), where \( f \) is the workload's mean hourly demand over about a week, and at least 10 millicores: a quiet weekend day is judged against the demand of a typical day, and a workload that idles at a few millicores isn't judged on its noise.

- **Daily confidence** is scored over the last 24 observed hours
- **Weekly confidence** is scored over the last 168 observed hours: a whole week of weekdays and weekend must be forecast well, not just the day just gone
- Each needs its full window before it reports a score

## Anomaly Detection

Traffic patterns change. A marketing campaign, a new feature launch, or a seasonal shift can invalidate what the model has learned.

Hybernate detects these **regime changes** using z-score anomaly detection:

1. Each hour, the signed prediction error \( e(t) = Y(t) - F(t) \) is converted to a two-sided z-score using an exponentially weighted mean \( \mu \) and standard deviation \( \sigma \) of recent errors, which remember about a week:

    \[
    z(t) = \frac{|e(t) - \mu|}{\max(\sigma, f)}
    \]

    A sudden surge and a sudden disappearance of demand both count.

2. If \( z(t) > 3.0 \), the observation is flagged as an anomaly. The model learns from it only up to 3 standard deviations, so a single spike isn't learned as a pattern
3. If 3 or more anomalies occur within 24 observed hours, the engine declares a **regime change**

On a regime change, the engine demotes its phase once:

- `FullyActive` → `WeeklySuggesting`
- `WeeklySuggesting` or `DailyActive` → `DailySuggesting`
- `DailySuggesting` → `Observing`

It then discards the evidence its confidence rested on: the scored errors, the anomaly statistics, and which hours of the week have been observed. Confidence has to be earned again on the new pattern, over a full window, before the forecast drives decisions again. A `RegimeChange` warning event is emitted once.

## Persistence

The engine's learned state (the model's components, the scored errors, the anomaly statistics, and the last hour observed) is stored in the ManagedWorkload's status, in `status.prediction.state`: gzipped JSON, base64-encoded, about 2.5 KB. It is written with each hourly observation. This means:

- The engine survives operator restarts and leader changes, and the hour observed just before a restart isn't observed twice
- No external database, persistent volume, or ConfigMap is needed
- Each workload has its own independent engine

State that can't be read, from an older version or a hand edit, is discarded with a `ForecastReset` warning event, and the engine starts learning again.

## Wall-Clock Alignment

Seasonal slots are keyed to wall-clock time, not to a running counter. The hour of day and day of week are counted in the operator's timezone (`--timezone`, UTC by default), so business hours stay in their slots when the clocks change for daylight saving. This means:

- If a workload is paused for 6 hours, the model doesn't lose alignment. The next observation goes into the correct hour-of-day slot
- While a workload is paused behind the [doorman](wake-on-request.md) (`WakeOnRequest=True`), each paused hour is recorded as zero demand: a request would have woken it, so an hour without one is an hour nobody asked for it. That's how the model learns that nights and weekends are quiet. A workload paused without the doorman records nothing while paused, since demand it can't see isn't zero
- Monday 9am always maps to the same slot, regardless of gaps

## Tuning

The default smoothing parameters work well for most workloads:

| Parameter | Default | Effect of increasing |
|-----------|---------|---------------------|
| \( \alpha \) | 0.01 | Level reacts faster to demand changes |
| \( \beta \) | 0.001 | Trend detection is more aggressive |
| \( \gamma_1 \) | 0.1 | Daily pattern adapts faster |
| \( \gamma_2 \) | 0.5 | Weekly pattern adapts faster |

Lower values produce more stable, smoother forecasts. Higher values make the model more reactive but noisier. A weekly component is updated once a week, so \( \gamma_2 \) is high enough for a weekday/weekend pattern to be learned in about three weeks; the defaults were chosen by simulating office-hours, always-on, idle, and noisy workloads.
