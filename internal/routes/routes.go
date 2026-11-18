// Package routes reads the operator's list of successful requests to ignore.
package routes

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const Path = "/rules/routes.txt"

type Rules map[string][]string

var segment = regexp.MustCompile(`^(?:[A-Za-z0-9._~-]+|\{[A-Za-z_][A-Za-z_0-9]*\})$`)

func Load(path string) (Rules, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open route rules: %w", err)
	}
	defer file.Close()
	rules := Rules{}
	scanner := bufio.NewScanner(file)
	for n := 1; scanner.Scan(); n++ {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !validMethod(fields[0]) || !validPattern(fields[1]) {
			return nil, fmt.Errorf("invalid route rule on line %d", n)
		}
		rules[fields[0]] = append(rules[fields[0]], fields[1])
	}
	return rules, scanner.Err()
}

func validMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT":
		return true
	}
	return false
}

func validPattern(path string) bool {
	if !unambiguous(path) {
		return false
	}
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part != "" && !segment.MatchString(part) {
			return false
		}
	}
	return true
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

func (rules Rules) Trigger(method, path string) bool {
	if unambiguous(path) {
		for _, pattern := range rules[method] {
			if matches(pattern, path) {
				return false
			}
		}
	}
	return true
}
