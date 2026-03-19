# Forecasting

Hybernate uses a **Holt-Winters double seasonal model** to learn your workload's demand patterns and predict future resource needs. This page covers the mathematical model, how it builds confidence over time, and how it handles pattern shifts.

## The Idea

Most workloads follow predictable patterns:

- **Daily**: Traffic peaks during business hours, drops overnight
- **Weekly**: Weekdays are busier than weekends

Hybernate learns these two patterns independently by feeding hourly CPU observations into a Holt-Winters model. Once it has enough data and confidence, it uses the learned patterns to:

- Confirm that a workload is genuinely idle (not just in a temporary lull)
- Recommend how many replicas a workload needs in the next hour

## How It Works

The model tracks four components:

| Component | What it represents | Smoothing parameter |
|-----------|-------------------|-------------------|
| **Level** | The baseline demand right now | \( \alpha = 0.1 \) |
| **Trend** | Whether demand is growing or shrinking | \( \beta = 0.01 \) |
| **Daily factors** | 24 multipliers, one per hour of day | \( \gamma_1 = 0.05 \) |
| **Weekly factors** | 168 multipliers, one per hour of week | \( \gamma_2 = 0.01 \) |

Each hour, the model records the actual CPU observation \( Y(t) \), compares it to what it predicted (for confidence scoring), and updates all four components using exponential smoothing:

\[
\begin{aligned}
L(t) &= \alpha \cdot \frac{Y(t)}{D(t) \cdot W(t)} + (1 - \alpha) \cdot \bigl(L(t{-}1) + T(t{-}1)\bigr) \\[6pt]
T(t) &= \beta \cdot \bigl(L(t) - L(t{-}1)\bigr) + (1 - \beta) \cdot T(t{-}1) \\[6pt]
D(t) &= \gamma_1 \cdot \frac{Y(t)}{L(t) \cdot W(t)} + (1 - \gamma_1) \cdot D(t - s_1) \\[6pt]
W(t) &= \gamma_2 \cdot \frac{Y(t)}{L(t) \cdot D(t)} + (1 - \gamma_2) \cdot W(t - s_2)
\end{aligned}
\]

Where \( s_1 = 24 \) (daily season length) and \( s_2 = 168 \) (weekly season length).

The forecast for \( h \) hours ahead is:

\[
F(t+h) = \bigl(L(t) + h \cdot T(t)\bigr) \times D(t+h \bmod s_1) \times W(t+h \bmod s_2)
\]

## Phase Lifecycle

The engine doesn't start making decisions immediately. It progresses through phases as it collects data and builds confidence:

`Observing` → `DailySuggesting` → `DailyActive` → `WeeklySuggesting` → `FullyActive`

| Phase | Requires | Behavior |
|-------|----------|----------|
| **Observing** | < 24 data points | Collecting data. No predictions yet. |
| **DailySuggesting** | 24+ data points | Daily predictions available but not yet trusted. Dry run is always forced. Predictions are logged but never applied. |
| **DailyActive** | Daily confidence >= threshold | Daily predictions drive decisions. Weekly patterns still learning. |
| **WeeklySuggesting** | 168+ data points | Weekly predictions available but not yet trusted. |
| **FullyActive** | Weekly confidence >= threshold | Both daily and weekly patterns drive decisions. |

The confidence threshold is configurable per workload via `spec.prediction.confidence` (default: 85%).

## Confidence Scoring

Confidence is measured using **Mean Absolute Percentage Error (MAPE)** over a rolling 24-hour window:

\[
C = 1 - \frac{1}{n} \sum_{i=1}^{n} \frac{|F(i) - Y(i)|}{Y(i)}
\]

A confidence of 85% means predictions are on average within 15% of actual values.

- Confidence starts at 0% and climbs as the model learns
- The scorer needs a full 24-hour window before it reports a meaningful score
- Each season (daily and weekly) has its own independent confidence score

## Anomaly Detection

Traffic patterns change. A marketing campaign, a new feature launch, or a seasonal shift can invalidate what the model has learned.

Hybernate detects these **regime changes** using z-score anomaly detection:

1. Each hour, the prediction error is converted to a z-score using the running mean \( \mu \) and standard deviation \( \sigma \) (computed via Welford's online algorithm):

    \[
    z(t) = \frac{|Y(t) - F(t)| - \mu}{\sigma}
    \]

2. If \( z(t) > 3.0 \), the observation is flagged as an anomaly
3. If 3 or more anomalies occur within a 24-hour window, the engine declares a **regime change**

On regime change, the engine demotes its phase:

- `FullyActive` → `WeeklySuggesting`
- `WeeklySuggesting` → `DailySuggesting`
- `DailySuggesting` → `Observing`

This forces the model to re-earn confidence with the new pattern before making decisions again.

## Persistence

The entire engine state (model parameters, seasonal factors, confidence scores, anomaly detector) is serialized to JSON and stored in the ManagedWorkload CR's status. This means:

- The engine survives operator restarts
- No external database or persistent volume is needed
- Each workload has its own independent engine

## Wall-Clock Alignment

Seasonal slots are keyed to wall-clock time (UTC), not to a running counter. This means:

- If a workload is paused for 6 hours, the model doesn't lose alignment. The next observation goes into the correct hour-of-day slot
- Monday 9am always maps to the same slot, regardless of gaps

## Tuning

The default smoothing parameters work well for most workloads:

| Parameter | Default | Effect of increasing |
|-----------|---------|---------------------|
| \( \alpha \) | 0.1 | Level reacts faster to demand changes |
| \( \beta \) | 0.01 | Trend detection is more aggressive |
| \( \gamma_1 \) | 0.05 | Daily pattern adapts faster |
| \( \gamma_2 \) | 0.01 | Weekly pattern adapts faster |

Lower values produce more stable, smoother forecasts. Higher values make the model more reactive but noisier. The defaults are conservative and prioritize stability over reactivity.
