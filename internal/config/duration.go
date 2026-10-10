package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts day ("3d") and week ("2w")
// units and is written back in the largest whole unit.
type Duration time.Duration

// MaxDuration is the longest duration any setting accepts: 10 years.
const MaxDuration = 10 * 365 * 24 * time.Hour

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ParseDuration parses Go durations plus d and w units, e.g. "90d", "1w2d", "36h".
// Negative values and values above MaxDuration are errors.
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("the value %q is not a duration. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.", s)
		}
		j := i
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') && rest[j] != '.' {
			j++
		}
		num, unit := rest[:i], rest[i:j]
		rest = rest[j:]
		var d time.Duration
		switch unit {
		case "d", "w":
			n, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("the value %q is not a duration. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.", s)
			}
			mult := 24 * time.Hour
			if unit == "w" {
				mult *= 7
			}
			if n*float64(mult) > float64(MaxDuration) {
				return 0, fmt.Errorf("the duration %q is longer than the maximum of 10 years.", s)
			}
			d = time.Duration(n * float64(mult))
		default:
			var err error
			if d, err = time.ParseDuration(num + unit); err != nil {
				return 0, fmt.Errorf("the value %q is not a duration. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.", s)
			}
		}
		if d > MaxDuration-total {
			return 0, fmt.Errorf("the duration %q is longer than the maximum of 10 years.", s)
		}
		total += d
	}
	return Duration(total), nil
}

// String formats the duration using d for whole days.
func (d Duration) String() string {
	td := time.Duration(d)
	if td == 0 {
		return "0"
	}
	day := 24 * time.Hour
	if td%day == 0 {
		return fmt.Sprintf("%dd", td/day)
	}
	if td%time.Hour == 0 {
		return fmt.Sprintf("%dh", td/time.Hour)
	}
	if td%time.Minute == 0 {
		return fmt.Sprintf("%dm", td/time.Minute)
	}
	return td.String()
}

// MarshalYAML writes the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON reads a duration string or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		*d = Duration(n)
		return nil
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Words writes the duration as digits and a unit word for sentences:
// "5 seconds", "10 minutes", "1 hour", "7 days".
func (d Duration) Words() string {
	v := d.D()
	unit := func(n int64, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	switch {
	case v >= 24*time.Hour && v%(24*time.Hour) == 0:
		return unit(int64(v/(24*time.Hour)), "day")
	case v >= time.Hour && v%time.Hour == 0:
		return unit(int64(v/time.Hour), "hour")
	case v >= time.Minute && v%time.Minute == 0:
		return unit(int64(v/time.Minute), "minute")
	default:
		return unit(int64(v/time.Second), "second")
	}
}
