// Package resource parses and formats Kubernetes CPU and memory quantities.
package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

func Parse(value string) (float64, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, err
	}
	return q.AsApproximateFloat64(), nil
}

func Format(value float64) string {
	if math.IsNaN(value) {
		return "?"
	}
	if value < 1 {
		return strconv.FormatInt(int64(value*1000), 10) + "m"
	}
	if value < 1024 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if math.Trunc(value) == value {
			text += ".0"
		}
		return text
	}
	units := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}
	integer := int64(value)
	for i, unit := range units {
		next := math.Pow(1024, float64(i+1))
		if float64(integer) < next || i == len(units)-1 || float64(integer)/next < 10 {
			return fmt.Sprintf("%.0f%s", float64(integer)/math.Pow(1024, float64(i)), unit)
		}
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 6, 64), "0"), ".")
}

func Diff(current, recommended float64, multiplier int) string {
	d := (recommended - current) * float64(multiplier)
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	return fmt.Sprintf("%s%s", sign, Format(d))
}
