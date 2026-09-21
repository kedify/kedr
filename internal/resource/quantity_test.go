package resource

import (
	"math"
	"testing"
)

func TestFormatCPU(t *testing.T) {
	for _, tt := range []struct {
		value float64
		want  string
	}{
		{0, "0m"}, {.004, "4m"}, {.027, "27m"}, {.0271234567, "27m"},
		{.17763329176625445, "178m"}, {1.0417997505975267, "1042m"},
		{2, "2000m"}, {-.073, "-73m"}, {-1.45744752566769, "-1457m"},
		{.54249, "542m"}, {.5425, "543m"}, {.54255, "543m"},
		{-.54255, "-543m"}, {.0005, "1m"}, {-.0005, "-1m"},
		{.000006, "0m"}, {-.0000001, "0m"}, {2048, "2048000m"},
		{math.NaN(), "?"}, {math.Inf(1), "?"}, {math.MaxFloat64, "?"},
	} {
		if got := FormatCPU(tt.value); got != tt.want {
			t.Errorf("FormatCPU(%g) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

func TestFormatMemory(t *testing.T) {
	for _, tt := range []struct {
		value float64
		want  string
	}{
		{0, "0"}, {512, "512"}, {1024, "1024"}, {10240, "10Ki"},
		{4000000, "3906Ki"}, {183.0880859375 * 1024 * 1024, "183Mi"},
		{15.1234567 * 1024 * 1024 * 1024, "15Gi"},
		{183.5 * 1024 * 1024, "184Mi"}, {-183.5 * 1024 * 1024, "-184Mi"},
		{-73 * 1024 * 1024, "-73Mi"},
		{math.NaN(), "?"}, {math.Inf(1), "?"},
	} {
		if got := FormatMemory(tt.value); got != tt.want {
			t.Errorf("FormatMemory(%g) = %q, want %q", tt.value, got, tt.want)
		}
	}
}
