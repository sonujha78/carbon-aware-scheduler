package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Hourly carbon intensity in gCO2/kWh: low at night, solar dip at midday, evening peak.
var curve = [24]float64{
	180, 170, 165, 160, 165, 190, 240, 300,
	320, 280, 220, 180, 150, 140, 150, 190,
	260, 340, 380, 360, 320, 270, 230, 200,
}

var (
	start      = time.Now()
	secPerHour = 60.0 // 1 simulated hour = 60 real seconds
	mu         sync.RWMutex
	override   *float64
)

func simHour() float64 {
	return math.Mod(time.Since(start).Seconds()/secPerHour, 24)
}

func currentIntensity() float64 {
	mu.RLock()
	defer mu.RUnlock()
	if override != nil {
		return *override
	}
	h := simHour()
	i := int(h)
	frac := h - float64(i)
	a, b := curve[i], curve[(i+1)%24]
	return a + (b-a)*frac
}

func main() {
	if v := os.Getenv("SIM_HOUR_SECONDS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			secPerHour = f
		}
	}

	http.HandleFunc("/intensity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"intensity":      math.Round(currentIntensity()*10) / 10,
			"unit":           "gCO2/kWh",
			"simulated_hour": math.Round(simHour()*100) / 100,
			"timestamp":      time.Now().UTC().Format(time.RFC3339),
		})
	})

	// /set?value=400 forces a value, /set?value=reset returns to the curve.
	http.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("value")
		mu.Lock()
		defer mu.Unlock()
		if v == "reset" {
			override = nil
			w.Write([]byte("reset to curve\n"))
			return
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, "bad value", http.StatusBadRequest)
			return
		}
		override = &f
		w.Write([]byte("override set\n"))
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	log.Println("mock carbon API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
