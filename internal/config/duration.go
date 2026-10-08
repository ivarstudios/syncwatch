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

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ParseDuration parses Go durations plus d and w units, e.g. "90d", "1w2d", "36h".
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
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		j := i
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') && rest[j] != '.' {
			j++
		}
		num, unit := rest[:i], rest[i:j]
		rest = rest[j:]
		switch unit {
		case "d", "w":
			n, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			mult := 24 * time.Hour
			if unit == "w" {
				mult *= 7
			}
			total += time.Duration(n * float64(mult))
		default:
			d, err := time.ParseDuration(num + unit)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			total += d
		}
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
