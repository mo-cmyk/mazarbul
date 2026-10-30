package ontology_test

import (
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func tsAt(superseder string, year int) ontology.Tombstone {
	return ontology.Tombstone{
		SupersededByUID: superseder,
		RetractedAt:     time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestReduceRetractions(t *testing.T) {
	t.Parallel()

	bound := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		extracted     *time.Time
		tombstones    []ontology.Tombstone
		wantSuperUID  string // "" means nil
		wantValidYear int    // 0 means nil
	}{
		{
			name:          "open relation no tombstones no bound",
			wantSuperUID:  "",
			wantValidYear: 0,
		},
		{
			name:          "extracted bound only stays open at the bound",
			extracted:     &bound,
			wantSuperUID:  "",
			wantValidYear: 2020,
		},
		{
			name:          "single tombstone reduces to its values",
			tombstones:    []ontology.Tombstone{tsAt("01HREL00000000000000AAAA", 2025)},
			wantSuperUID:  "01HREL00000000000000AAAA",
			wantValidYear: 2025,
		},
		{
			name: "distinct superseders converge to lowest ULID and earliest close",
			tombstones: []ontology.Tombstone{
				tsAt("01HREL00000000000000BBBB", 2026),
				tsAt("01HREL00000000000000AAAA", 2025),
			},
			wantSuperUID:  "01HREL00000000000000AAAA",
			wantValidYear: 2025,
		},
		{
			name: "order independence reversed input same output",
			tombstones: []ontology.Tombstone{
				tsAt("01HREL00000000000000AAAA", 2025),
				tsAt("01HREL00000000000000BBBB", 2026),
			},
			wantSuperUID:  "01HREL00000000000000AAAA",
			wantValidYear: 2025,
		},
		{
			name:      "extracted bound earlier than retractions wins for valid until",
			extracted: &bound,
			tombstones: []ontology.Tombstone{
				tsAt("01HREL00000000000000AAAA", 2025),
			},
			wantSuperUID:  "01HREL00000000000000AAAA",
			wantValidYear: 2020,
		},
		{
			name: "pure forget tombstone leaves superseder nil but closes valid until",
			tombstones: []ontology.Tombstone{
				{SupersededByUID: "", RetractedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
			wantSuperUID:  "",
			wantValidYear: 2025,
		},
		{
			name: "mixed forget and supersede picks the supersede pointer and earliest close",
			tombstones: []ontology.Tombstone{
				{SupersededByUID: "", RetractedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
				tsAt("01HREL00000000000000CCCC", 2025),
			},
			wantSuperUID:  "01HREL00000000000000CCCC",
			wantValidYear: 2024,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSuper, gotValid := ontology.ReduceRetractions(tc.extracted, tc.tombstones)

			switch {
			case tc.wantSuperUID == "" && gotSuper != nil:
				t.Errorf("supersededByUID = %q, want nil", *gotSuper)
			case tc.wantSuperUID != "" && gotSuper == nil:
				t.Errorf("supersededByUID = nil, want %q", tc.wantSuperUID)
			case tc.wantSuperUID != "" && *gotSuper != tc.wantSuperUID:
				t.Errorf("supersededByUID = %q, want %q", *gotSuper, tc.wantSuperUID)
			}

			switch {
			case tc.wantValidYear == 0 && gotValid != nil:
				t.Errorf("validUntil = %v, want nil", *gotValid)
			case tc.wantValidYear != 0 && gotValid == nil:
				t.Errorf("validUntil = nil, want year %d", tc.wantValidYear)
			case tc.wantValidYear != 0 && gotValid.Year() != tc.wantValidYear:
				t.Errorf("validUntil year = %d, want %d", gotValid.Year(), tc.wantValidYear)
			case tc.wantValidYear != 0 && gotValid.Location() != time.UTC:
				t.Errorf("validUntil not UTC: %v", gotValid.Location())
			}
		})
	}
}
