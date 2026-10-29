package edc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseDarwinGPUBusyPicksTheBusiestAccelerator(t *testing.T) {
	output := `+-o AGXAcceleratorG16X  <class AGXAcceleratorG16X>
    "PerformanceStatistics" = {"In use system memory"=1,"Device Utilization %"=12,"Renderer Utilization %"=10}
+-o AGXAcceleratorG16X  <class AGXAcceleratorG16X>
    "PerformanceStatistics" = {"Device Utilization %"=48}`
	if busy, ok := parseDarwinGPUBusy(output); !ok || busy != 48 {
		t.Fatalf("busy = %v, %v", busy, ok)
	}
	if _, ok := parseDarwinGPUBusy("+-o IOAccelerator\n"); ok {
		t.Fatal("output without utilization reported a GPU value")
	}
}

func TestTopHeatSignalsFlowFromSnapshotToSampleAndPanel(t *testing.T) {
	previous := resourceSnapshot{TakenAt: time.Unix(0, 0), ThrottleMS: 1000, ThrottleValid: true}
	current := resourceSnapshot{TakenAt: time.Unix(2, 0), ThrottleMS: 1500, ThrottleValid: true, Thermal: topThermalHeavy, TempC: 91.5, TempValid: true, GPUBusy: 48, GPUValid: true}
	rate := calculateRate(previous, current)
	if rate.ThrottlePercent != 25 || !rate.ThrottleValid {
		t.Fatalf("throttle = %v %v", rate.ThrottlePercent, rate.ThrottleValid)
	}
	if text := topHeatText(rate); text != "thermal heavy throttle 25% 92°C gpu 48%" {
		t.Fatalf("heat text = %q", text)
	}
	data, err := json.Marshal(newTopSample(hostDetails{}, current.TakenAt, rate))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"thermal":"heavy"`, `"cpu_throttle_pct":25`, `"temp_c":91.5`, `"gpu_busy_pct":48`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("sample %s misses %s", data, want)
		}
	}
	// 읽지 못한 값은 0이 아니라 키가 빠진다.
	data, _ = json.Marshal(newTopSample(hostDetails{}, current.TakenAt, calculateRate(resourceSnapshot{}, resourceSnapshot{TakenAt: time.Unix(1, 0)})))
	for _, key := range []string{"thermal", "cpu_throttle_pct", "temp_c", "gpu_busy_pct"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("unsupported %s is in %s", key, data)
		}
	}
}

func TestTopThermalSignalWarnsFromHeavy(t *testing.T) {
	limits := newTopLimits(8, true)
	for thermal, want := range map[topThermal]string{topThermalModerate: "", topThermalHeavy: "thermal heavy", topThermalTrapping: "thermal trapping"} {
		found := ""
		for _, item := range topSignals(resourceRate{Thermal: thermal}, limits) {
			if strings.HasPrefix(item.text, "thermal") {
				found = item.text
			}
		}
		if found != want {
			t.Fatalf("%v signal = %q, want %q", thermal, found, want)
		}
	}
}
