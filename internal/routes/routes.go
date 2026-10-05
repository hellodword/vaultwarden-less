// Package routes keeps request exclusions separate from notification cataloging.
package routes

import (
	_ "embed"
	"math"
	"strings"
)

//go:embed catalog.txt
var catalogText string

var catalog = lines(catalogText)
var exclusions = lines(excludedText)

func lines(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func unambiguous(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%\\\x00") || strings.Contains(path, "//") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func matches(pattern, path string) bool {
	left, right := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(left) != len(right) {
		return false
	}
	for i, part := range left {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			if right[i] == "" {
				return false
			}
		} else if part != right[i] {
			return false
		}
	}
	return true
}

func Trigger(method, path string) bool {
	if !unambiguous(path) {
		return true
	}
	for _, line := range exclusions {
		m, p, _ := strings.Cut(line, " ")
		if method == m && matches(p, path) {
			return false
		}
	}
	return true
}

func Classify(method, path string) (string, string) {
	if unambiguous(path) {
		best := ""
		specificity := -1
		for _, line := range catalog {
			m, p, _ := strings.Cut(line, " ")
			if m != method || !matches(p, path) {
				continue
			}
			score := 0
			for _, part := range strings.Split(p, "/") {
				if !strings.HasPrefix(part, "{") {
					score++
				}
			}
			if score > specificity {
				best = line
				specificity = score
			}
		}
		if best != "" {
			return best, ""
		}
	}
	return "", Method(method)
}

func Method(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT":
		return method
	}
	return "OTHER"
}

type Counts struct {
	Started  uint64 `json:"started"`
	Finished uint64 `json:"finished"`
}
type Summary struct {
	Routes  map[string]Counts `json:"routes"`
	Unknown map[string]Counts `json:"unknown"`
}

func add(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func (s *Summary) Record(method, path string, start bool) {
	route, unknown := Classify(method, path)
	if s.Routes == nil {
		s.Routes = map[string]Counts{}
	}
	if s.Unknown == nil {
		s.Unknown = map[string]Counts{}
	}
	destination, key := s.Routes, route
	if route == "" {
		destination, key = s.Unknown, unknown
	}
	count := destination[key]
	if start {
		count.Started = add(count.Started, 1)
	} else {
		count.Finished = add(count.Finished, 1)
	}
	destination[key] = count
}
func (s *Summary) Merge(other Summary) {
	merge := func(dst *map[string]Counts, src map[string]Counts) {
		if *dst == nil {
			*dst = map[string]Counts{}
		}
		for k, v := range src {
			old := (*dst)[k]
			old.Started = add(old.Started, v.Started)
			old.Finished = add(old.Finished, v.Finished)
			(*dst)[k] = old
		}
	}
	merge(&s.Routes, other.Routes)
	merge(&s.Unknown, other.Unknown)
}
func (s Summary) Empty() bool { return len(s.Routes) == 0 && len(s.Unknown) == 0 }
func (s Summary) Valid() bool {
	for route := range s.Routes {
		found := false
		for _, known := range catalog {
			if route == known {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for method := range s.Unknown {
		if Method(method) != method {
			return false
		}
	}
	return true
}
