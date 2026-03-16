/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package forecast

const stateVersion = 1

// EngineState is the top-level serialization format for persisting a forecast
// engine to the CR status. JSON keys are short to minimize etcd storage.
type EngineState struct {
	Version int          `json:"v"`
	Model   ModelState   `json:"m"`
	Daily   ScorerState  `json:"ds"`
	Weekly  ScorerState  `json:"ws"`
	Anomaly AnomalyState `json:"ad"`
	Phase   int          `json:"p"`
	Thresh  int          `json:"th"`
	DDemote bool         `json:"dd"`
	WDemote bool         `json:"wd"`
}

type ModelState struct {
	Alpha  float64              `json:"a"`
	Beta   float64              `json:"b"`
	Gamma1 float64              `json:"g1"`
	Gamma2 float64              `json:"g2"`
	Level  float64              `json:"l"`
	Trend  float64              `json:"t"`
	Daily  [DailySeason]float64 `json:"d"`
	Weekly [WeeklySeason]float64 `json:"w"`
	N      int                  `json:"n"`
}

type ScorerState struct {
	Window int       `json:"w"`
	Errors []float64 `json:"e"`
	Pos    int       `json:"p"`
	Full   bool      `json:"f"`
}

type AnomalyState struct {
	Errors []float64 `json:"e"`
	Pos    int       `json:"p"`
	Full   bool      `json:"f"`
	Mean   float64   `json:"m"`
	M2     float64   `json:"m2"`
	Count  int       `json:"c"`
	Recent []bool    `json:"r"`
	RPos   int       `json:"rp"`
	RFull  bool      `json:"rf"`
}
