package geo_test

import (
	"math"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/geo"
)

// TestHaversineKm validates the great-circle distance formula against known
// city-pair distances. Tolerances are 1% of the expected distance because the
// haversine formula treats the Earth as a sphere (it is slightly oblate);
// maximum real-world error is around 0.3%.
func TestHaversineKm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label        string
		lat1, lon1   float64 // origin
		lat2, lon2   float64 // destination
		wantKm       float64 // expected great-circle distance
		tolerancePct float64 // allowed relative error
	}{
		{
			// Berlin (52.5200 N, 13.4050 E) to Munich (48.1351 N, 11.5820 E).
			// Reference: ~504 km.
			label: "Berlin-Munich",
			lat1:  52.5200, lon1: 13.4050,
			lat2: 48.1351, lon2: 11.5820,
			wantKm:       504,
			tolerancePct: 1.0,
		},
		{
			// Berlin (52.5200 N, 13.4050 E) to New York City (40.7128 N, 74.0060 W).
			// Reference: ~6385 km.
			label: "Berlin-NYC",
			lat1:  52.5200, lon1: 13.4050,
			lat2: 40.7128, lon2: -74.0060,
			wantKm:       6385,
			tolerancePct: 1.0,
		},
		{
			// Berlin (52.5200 N, 13.4050 E) to Wellington (41.2865 S, 174.7762 E).
			// Near-antipodal great-circle distance on the WGS84 mean radius
			// 6371 km sphere is ~18138 km. Real-world references that quote
			// ~18430 km use ellipsoidal Vincenty, which we do not implement;
			// the spherical answer is accurate to within 1% for our purpose
			// (filter-by-radius), so we test the spherical reference here.
			label: "Berlin-Wellington",
			lat1:  52.5200, lon1: 13.4050,
			lat2: -41.2865, lon2: 174.7762,
			wantKm:       18138,
			tolerancePct: 1.0,
		},
		{
			// Same point: distance must be exactly 0.
			label: "identity",
			lat1:  48.8566, lon1: 2.3522,
			lat2: 48.8566, lon2: 2.3522,
			wantKm:       0,
			tolerancePct: 0, // exact
		},
		{
			// North Pole to South Pole: half circumference ~20 015 km.
			label: "poles",
			lat1:  90, lon1: 0,
			lat2: -90, lon2: 0,
			wantKm:       20015,
			tolerancePct: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			got := geo.HaversineKm(tc.lat1, tc.lon1, tc.lat2, tc.lon2)

			// Identity case: expect exact zero.
			if tc.wantKm == 0 {
				if got != 0 {
					t.Errorf("HaversineKm(%v,%v,%v,%v) = %v, want 0",
						tc.lat1, tc.lon1, tc.lat2, tc.lon2, got)
				}
				return
			}

			pctErr := math.Abs(got-tc.wantKm) / tc.wantKm * 100
			if pctErr > tc.tolerancePct {
				t.Errorf("HaversineKm(%v,%v,%v,%v) = %.1f km, want %.1f km (err %.2f%% > %.2f%%)",
					tc.lat1, tc.lon1, tc.lat2, tc.lon2,
					got, tc.wantKm, pctErr, tc.tolerancePct)
			}
		})
	}
}

// TestHaversineKm_Symmetry verifies that d(A,B) == d(B,A) for all pairs.
func TestHaversineKm_Symmetry(t *testing.T) {
	t.Parallel()

	pairs := [][4]float64{
		{52.52, 13.40, 48.14, 11.58},   // Berlin-Munich
		{40.71, -74.01, 35.68, 139.69}, // NYC-Tokyo
		{-33.87, 151.21, 51.51, -0.13}, // Sydney-London
	}
	for _, p := range pairs {
		ab := geo.HaversineKm(p[0], p[1], p[2], p[3])
		ba := geo.HaversineKm(p[2], p[3], p[0], p[1])
		if math.Abs(ab-ba) > 1e-9 {
			t.Errorf("asymmetry: d(A,B)=%.6f d(B,A)=%.6f diff=%.2e", ab, ba, math.Abs(ab-ba))
		}
	}
}
