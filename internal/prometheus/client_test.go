package prometheus

import (
	"encoding/json"
	"math"
	"testing"
)

func TestSamplePairDecoding(t *testing.T) {
	for _, raw := range []string{
		`[1790000000.125,"1.234e-5"]`,
		`[ 1790000000.125 , "123.456" ]`,
		`[1,"\u0031.25"]`,
		`[1,"NaN"]`, `[1,"+Inf"]`, `[1,"-Inf"]`, `[1,"-1"]`,
	} {
		t.Run(raw, func(t *testing.T) {
			var wantRaw []json.RawMessage
			if err := json.Unmarshal([]byte(raw), &wantRaw); err != nil {
				t.Fatal(err)
			}
			want, err := parsePair(wantRaw)
			if err != nil {
				t.Fatal(err)
			}
			var got samplePair
			if err = json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Time != want.Time || got.Value != want.Value && !(math.IsNaN(got.Value) && math.IsNaN(want.Value)) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if got.Value < 0 || math.IsNaN(got.Value) || math.IsInf(got.Value, 0) {
				if _, err := nativeSamples([]series{{Values: []samplePair{got}}}); err == nil {
					t.Fatal("invalid usage sample accepted")
				}
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `[1]`, `[1,2]`, `[1,"2",3]`, `["1","2"]`, `[null,"2"]`, `[1,"bad"]`, `[1,"2]`} {
		var got samplePair
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("accepted malformed sample %s", raw)
		}
	}
}
