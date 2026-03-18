# Forecasting

Hybernate uses a **Holt-Winters double seasonal model** to learn your workload's demand patterns and predict future resource needs. This page explains how it works without requiring a statistics background.

## The Idea

Most workloads follow predictable patterns:

- **Daily**: Traffic peaks during business hours, drops overnight
- **Weekly**: Weekdays are busier than weekends

Hybernate learns these two patterns independently by feeding hourly CPU observations into a statistical model. Once it has enough data and confidence, it uses the learned patterns to:

- Confirm that a workload is genuinely idle (not just in a temporary lull)
- Recommend how many replicas a workload needs in the next hour

## How It Works

The model tracks four components:

| Component | What it represents | How fast it adapts |
|-----------|-------------------|-------------------|
| **Level** | The baseline demand right now | `alpha = 0.1` |
| **Trend** | Whether demand is growing or shrinking | `beta = 0.01` |
| **Daily factors** | 24 multipliers, one per hour of day | `gamma1 = 0.05` |
| **Weekly factors** | 168 multipliers, one per hour of week | `gamma2 = 0.01` |

Each hour, the model:

1. Records the actual CPU observation
2. Compares it to what it predicted (for confidence scoring)
3. Updates all four components using exponential smoothing

The forecast for `h` hours ahead is:

```
Forecast = (Level + h × Trend) × DailyFactor[target_hour] × WeeklyFactor[target_hour]
```

## Phase Lifecycle

The engine doesn't start making decisions immediately. It progresses through phases as it collects data and builds confidence:

```
Observing → DailySuggesting → DailyActive → WeeklySuggesting → FullyActive
```

| Phase | Requires | Behavior |
|-------|----------|----------|
| **Observing** | < 24 data points | Learning only. No predictions. |
| **DailySuggesting** | 24+ data points | Daily predictions available but not yet trusted. Shadow mode — predictions are scored but don't drive decisions. |
| **DailyActive** | Daily confidence >= threshold | Daily predictions drive decisions. Weekly patterns still learning. |
| **WeeklySuggesting** | 168+ data points | Weekly predictions available but not yet trusted. |
| **FullyActive** | Weekly confidence >= threshold | Both daily and weekly patterns drive decisions. |

The confidence threshold is configurable per workload via `spec.prediction.confidence` (default: 85%).

## Confidence Scoring

Confidence is measured using **Mean Absolute Percentage Error (MAPE)** over a rolling 24-hour window:

```
Confidence = 1 - average(|forecast - actual| / actual)
```

A confidence of 85% means predictions are on average within 15% of actual values.

- Confidence starts at 0% and climbs as the model learns
- The scorer needs a full 24-hour window before it reports a meaningful score
- Each season (daily and weekly) has its own independent confidence score

## Anomaly Detection

Traffic patterns change. A marketing campaign, a new feature launch, or a seasonal shift can invalidate what the model has learned.

Hybernate detects these **regime changes** using z-score anomaly detection:

1. Each hour, the prediction error (actual - forecast) is compared against the running mean and standard deviation
2. If the error exceeds 3 standard deviations, it's flagged as an anomaly
3. If 3 or more anomalies occur within a 24-hour window, the engine declares a **regime change**

On regime change, the engine demotes its phase:

- FullyActive → WeeklySuggesting
- WeeklySuggesting → DailySuggesting
- DailySuggesting → Observing

This forces the model to re-earn confidence with the new pattern before making decisions again.

## Persistence

The entire engine state (model parameters, seasonal factors, confidence scores, anomaly detector) is serialized to JSON and stored in the ManagedWorkload CR's status. This means:

- The engine survives operator restarts
- No external database or persistent volume is needed
- Each workload has its own independent engine

## Wall-Clock Alignment

Seasonal slots are keyed to wall-clock time (UTC), not to a running counter. This means:

- If a workload is paused for 6 hours, the model doesn't lose alignment — the next observation goes into the correct hour-of-day slot
- Monday 9am always maps to the same slot, regardless of gaps

## Tuning

The default smoothing parameters work well for most workloads:

| Parameter | Default | Effect of increasing |
|-----------|---------|---------------------|
| `alpha` | 0.1 | Level reacts faster to demand changes |
| `beta` | 0.01 | Trend detection is more aggressive |
| `gamma1` | 0.05 | Daily pattern adapts faster |
| `gamma2` | 0.01 | Weekly pattern adapts faster |

Lower values produce more stable, smoother forecasts. Higher values make the model more reactive but noisier. The defaults are conservative — they prioritize stability over reactivity.
