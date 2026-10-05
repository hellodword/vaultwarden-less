package notify

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/hellodword/vaultwarden-less/internal/routes"
)

type TargetStatus struct {
	Name, State                string
	Waiting, Uploading, Failed bool
}
type Event struct {
	Checks      uint64
	Count       uint64
	Undelivered uint64
	Reasons     map[string]uint64
	Routes      map[string]routes.Counts
	Unknown     map[string]routes.Counts
	Source      string
	Issues      map[string]int
	Targets     []TargetStatus
	Warnings    []string
}

func Add(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func (e Event) clone() Event {
	e.Reasons = maps.Clone(e.Reasons)
	e.Routes = maps.Clone(e.Routes)
	e.Unknown = maps.Clone(e.Unknown)
	e.Issues = maps.Clone(e.Issues)
	e.Targets = slices.Clone(e.Targets)
	e.Warnings = slices.Clone(e.Warnings)
	return e
}

// Merge keeps phase counts and the most recent state, not a request log.
// A nonempty Source carries the latest diagnostics, including recovery.
func (e *Event) Merge(next Event) {
	e.Count = Add(e.Count, next.Count)
	e.Checks = Add(e.Checks, next.Checks)
	if e.Reasons == nil {
		e.Reasons = map[string]uint64{}
	}
	for key, count := range next.Reasons {
		e.Reasons[key] = Add(e.Reasons[key], count)
	}
	activity := routes.Summary{Routes: e.Routes, Unknown: e.Unknown}
	activity.Merge(routes.Summary{Routes: next.Routes, Unknown: next.Unknown})
	e.Routes, e.Unknown = activity.Routes, activity.Unknown
	if next.Source != "" {
		e.Source = next.Source
		e.Issues = maps.Clone(next.Issues)
		e.Warnings = slices.Clone(next.Warnings)
	}
	for _, target := range next.Targets {
		found := false
		for i := range e.Targets {
			if e.Targets[i].Name == target.Name {
				e.Targets[i] = target
				found = true
				break
			}
		}
		if !found {
			e.Targets = append(e.Targets, target)
		}
	}
	e.Undelivered = Add(e.Undelivered, next.Undelivered)
}

func targetDescription(t TargetStatus) (description, category string) {
	switch {
	case t.State == "invalid":
		return "Invalid configuration", "error"
	case t.Failed || t.State == "failed":
		if t.Uploading {
			return "Retrying upload", "error"
		}
		return "Upload failed; retry pending", "error"
	case t.Uploading || t.State == "uploading":
		return "Uploading", "pending"
	case t.Waiting && t.State == "uploaded":
		return "Newer capture pending", "pending"
	case t.Waiting || t.State == "waiting":
		return "Waiting for upload", "pending"
	case t.State == "uploaded":
		return "Uploaded", "updated"
	case t.State == "unchanged":
		return "Up to date", "current"
	case t.State == "initializing":
		return "Checking repository", "pending"
	default:
		return "Pending", "pending"
	}
}

func (e Event) targetCounts() map[string]int {
	counts := map[string]int{}
	for _, target := range e.Targets {
		_, category := targetDescription(target)
		counts[category]++
	}
	return counts
}

// Title prioritizes source completeness over successful uploads of that source.
func (e Event) Title() string {
	switch e.Source {
	case "unconfigured":
		return "Backup not configured"
	case "failed":
		return "Capture failed"
	case "incomplete":
		return "Incomplete capture"
	}
	if len(e.Issues) > 0 {
		return "Incomplete capture"
	}
	counts := e.targetCounts()
	switch {
	case counts["error"] > 0:
		return "Backup target error"
	case len(e.Warnings) > 0:
		return "Backup warning"
	case e.Source != "complete" || len(e.Targets) == 0 || counts["pending"] > 0:
		return "Backup pending"
	case counts["updated"] > 0:
		return "Backup updated"
	default:
		return "No changes"
	}
}

func (e Event) sourceDescription() string {
	switch e.Source {
	case "complete":
		return "Complete"
	case "incomplete":
		return "Incomplete; retry pending"
	case "failed":
		return "Capture failed; retry pending"
	case "unconfigured":
		return "No valid backup target"
	default:
		return "Not checked yet"
	}
}

func (e Event) targetSummary() string {
	counts := e.targetCounts()
	var parts []string
	for _, category := range []string{"error", "pending", "updated", "current"} {
		if count := counts[category]; count > 0 {
			label := category
			if category == "current" {
				label = "up to date"
			}
			parts = append(parts, fmt.Sprintf("%d %s", count, label))
		}
	}
	if len(parts) == 0 {
		return "None"
	}
	return strings.Join(parts, " · ")
}

func (e Event) targetLabel() string {
	if e.Source == "failed" {
		return "Targets (previous capture)"
	}
	return "Targets"
}

// PlainSummary is also usable when the Bark notification extension cannot run.
// Do not put Markdown syntax or unbounded dynamic names in the APNs fallback.
func (e Event) PlainSummary() string {
	return "Source: " + e.sourceDescription() + ". " + e.targetLabel() + ": " + e.targetSummary() + "."
}

func markdownText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, value)
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\",
		"*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "#", "\\#", "!", "\\!", "|", "\\|",
		"~", "\\~", "-", "\\-", "+", "\\+", ".", "\\.",
	).Replace(value)
}

func quantity(n uint64, label string) string {
	if n != 1 {
		label += "s"
	}
	return fmt.Sprintf("%d %s", n, label)
}

func diagnosticDescription(code string) string {
	// Codes are produced locally. Never format errors from subprocesses or
	// remote servers, which may contain paths, request data or credentials.
	switch code {
	case "capture_timeout":
		return "Capture timed out"
	case "database_comparison_unavailable":
		return "Database comparison unavailable; retry pending"
	case "file_reference_diagnostics_unavailable":
		return "File reference diagnostics unavailable"
	case "baseline_cache_unavailable":
		return "Local baseline cache unavailable"
	default:
		return markdownText(strings.ReplaceAll(code, "_", " "))
	}
}

// Message uses the shared Markdown subset: bold labels and quoted paragraphs.
// Limits count UTF-8 bytes. Entire detail blocks are omitted; neither a rune
// nor a Markdown construct is ever cut in half. The summary is always retained.
func (e Event) Message(limit int) string {
	header := "**vaultwarden-less · " + e.Title() + "**\n\n**Source:** " + e.sourceDescription() +
		"\n\n**" + e.targetLabel() + ":** " + e.targetSummary()
	footer := "\n\n**Summary:** " + quantity(e.Checks, "check") + " · " + quantity(e.Count, "report")
	if e.Undelivered > 0 {
		footer += " · " + quantity(e.Undelivered, "undelivered report")
	}
	type detail struct{ section, text string }
	var details []detail
	for _, target := range e.Targets {
		description, _ := targetDescription(target)
		details = append(details, detail{"Target details", markdownText(target.Name) + ": " + description})
	}
	for _, code := range slices.Sorted(maps.Keys(e.Issues)) {
		details = append(details, detail{"Diagnostics", fmt.Sprintf("%s: %d", diagnosticDescription(code), e.Issues[code])})
	}
	for _, warning := range slices.Sorted(slices.Values(e.Warnings)) {
		d := detail{"Diagnostics", diagnosticDescription(warning)}
		if len(details) == 0 || details[len(details)-1] != d {
			details = append(details, d)
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(e.Reasons)) {
		label := map[string]string{
			"startup": "Startup check", "manual": "Manual check", "request": "Request activity",
			"scheduled": "Scheduled check", "scheduled_full": "Scheduled full check",
			"source_retry": "Source retry", "target_result": "Target result",
		}[reason]
		if label == "" {
			label = markdownText(reason)
		}
		details = append(details, detail{"Triggers", fmt.Sprintf("%s: %d", label, e.Reasons[reason])})
	}
	activity := func(prefix string, counts map[string]routes.Counts) {
		for _, name := range slices.Sorted(maps.Keys(counts)) {
			count := counts[name]
			details = append(details, detail{"Request activity", fmt.Sprintf("%s%s: started %d, finished %d", prefix, markdownText(name), count.Started, count.Finished)})
		}
	}
	activity("Unknown ", e.Unknown)
	activity("", e.Routes)
	message, section, omitted := header, "", 0
	for _, d := range details {
		block := "\n\n> " + d.text
		if d.section != section {
			block = "\n\n**" + d.section + "**" + block
		}
		// Reserve the complete omission marker before accepting any detail.
		if len(message)+len(block)+len(footer)+len(fmt.Sprintf("\n\nOmitted details: %d", len(details))) > limit {
			omitted++
			continue
		}
		message += block
		section = d.section
	}
	message += footer
	if omitted > 0 {
		message += fmt.Sprintf("\n\nOmitted details: %d", omitted)
	}
	return message
}
