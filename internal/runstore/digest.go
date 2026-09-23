package runstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

// MetricsDigest fingerprints normalized collected observations, with canonical
// ordering. Samples exist only during hashing and are never saved in the run.
func MetricsDigest(metrics strategy.Metrics) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, series := range [][]analysis.Series{metrics.CPU, metrics.Memory} {
		rows := append([]analysis.Series(nil), series...)
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		if err := enc.Encode(len(rows)); err != nil {
			return "", err
		}
		for _, row := range rows {
			values, err := analysis.NormalizeSamples(row, metrics.WindowStart, metrics.EvaluationTime)
			if err != nil {
				return "", err
			}
			row.Samples, row.Kind = values, analysis.SampleGauge
			if err = enc.Encode(row); err != nil {
				return "", err
			}
		}
	}
	events := append([]analysis.OOMKill(nil), metrics.OOMKills...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	if err := enc.Encode(events); err != nil {
		return "", err
	}
	return "normalized-v1:" + hex.EncodeToString(h.Sum(nil)), nil
}
